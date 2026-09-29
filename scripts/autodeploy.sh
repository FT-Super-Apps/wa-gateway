#!/usr/bin/env bash
# WA Gateway — auto-deploy berbasis PULL, berjalan di server produksi.
#
# Dijalankan systemd timer (wa-gateway-autodeploy.timer) tiap menit. Menarik image
# `wa-gateway` dari GHCR; bila digest tag yang dipakai compose (`:latest`)
# berubah, recreate service itu, lalu cek kesehatan. Idempoten — tanpa perubahan
# image tidak ada yang disentuh. Infra (postgres, minio) tidak pernah disentuh.
#
# Kenapa pull, bukan push (SSH dari GitHub Actions)? Jalur runner cloud →
# jump host → LAN kampus sering putus/timeout. Dengan pull, server hanya
# butuh koneksi KELUAR ke ghcr.io; CI cukup mendorong image.
#
# Env (diset di unit systemd, lihat scripts/install-autodeploy.sh):
#   DEPLOY_DIR   folder compose prod (default /home/muhyiddin/docker/wa-gateway)
#   SERVICES     service yang dipantau (default "wa-gateway")
#   LOG_FILE     log append (default /var/log/wa-gateway-autodeploy.log)
#   HEALTH_WAIT  detik menunggu gateway sehat setelah recreate (default 90)
set -uo pipefail

DEPLOY_DIR="${DEPLOY_DIR:-/home/muhyiddin/docker/wa-gateway}"
SERVICES="${SERVICES:-wa-gateway}"
LOG_FILE="${LOG_FILE:-/var/log/wa-gateway-autodeploy.log}"
HEALTH_WAIT="${HEALTH_WAIT:-90}"
LOCK_FILE="${LOCK_FILE:-/run/lock/wa-gateway-autodeploy.lock}"

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOG_FILE"; }

# Satu instance saja; bila tick sebelumnya masih berjalan, lewati tanpa ribut.
exec 9>"$LOCK_FILE"
flock -n 9 || exit 0

cd "$DEPLOY_DIR" || { log "ERROR: DEPLOY_DIR tidak ada: $DEPLOY_DIR"; exit 1; }

# ID image yang dipakai container berjalan (kosong bila container tidak ada).
running_image_id() {
  local cid
  cid="$(docker compose ps -q "$1" 2>/dev/null | head -n1)"
  [[ -n "$cid" ]] && docker inspect --format '{{.Image}}' "$cid" 2>/dev/null || true
}
# Referensi image service (mis. ghcr.io/…/wa-gateway:latest) — dari container bila
# ada, kalau tidak dari compose config.
service_image_ref() {
  local cid
  cid="$(docker compose ps -aq "$1" 2>/dev/null | head -n1)"
  if [[ -n "$cid" ]]; then
    docker inspect --format '{{.Config.Image}}' "$cid" 2>/dev/null && return
  fi
  docker compose config --format json 2>/dev/null | jq -r --arg s "$1" '.services[$s].image // empty'
}

# 1. Tarik image (senyap). Gagal → catat, coba lagi di tick berikutnya.
if ! pull_out="$(docker compose pull -q $SERVICES 2>&1)"; then
  log "WARN: pull gagal — ${pull_out##*$'\n'}"
  exit 0
fi

# 2. Bandingkan digest: container berjalan vs tag yang baru ditarik.
changed=()
for svc in $SERVICES; do
  ref="$(service_image_ref "$svc")"
  [[ -z "$ref" ]] && { log "WARN: image untuk service '$svc' tidak diketahui"; continue; }
  new_id="$(docker image inspect --format '{{.Id}}' "$ref" 2>/dev/null || true)"
  cur_id="$(running_image_id "$svc")"
  if [[ -n "$new_id" && "$new_id" != "$cur_id" ]]; then
    changed+=("$svc")
    log "image baru untuk $svc: ${cur_id:-<tidak berjalan>} → $new_id ($ref)"
  fi
done
[[ ${#changed[@]} -eq 0 ]] && exit 0

# 3. Recreate hanya service yang berubah (infra tidak disentuh).
log "deploy: recreate ${changed[*]}"
if ! docker compose up -d --no-deps "${changed[@]}" 2>&1 | tee -a "$LOG_FILE"; then
  log "ERROR: docker compose up gagal"
  exit 1
fi
docker image prune -f >/dev/null 2>&1 || true

# 4. Health check langsung ke port gateway (PORT dari .env, default 3000).
APP_PORT="$(grep -E '^PORT=' .env 2>/dev/null | cut -d= -f2 | tr -d '[:space:]')"
APP_PORT="${APP_PORT:-3000}"
ok=0
for ((i = 0; i < HEALTH_WAIT / 5; i++)); do
  if curl -sf "http://localhost:${APP_PORT}/health" >/dev/null 2>&1; then ok=1; break; fi
  sleep 5
done
if [[ $ok -ne 1 ]]; then
  log "ERROR: wa-gateway belum sehat setelah ${HEALTH_WAIT}s — periksa: docker compose logs wa-gateway"
  exit 1
fi
ver="$(curl -sf "http://localhost:${APP_PORT}/version" 2>/dev/null || echo '{}')"
log "OK: sehat — $ver"
