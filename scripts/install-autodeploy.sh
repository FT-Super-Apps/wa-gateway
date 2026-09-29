#!/usr/bin/env bash
# Pasang/perbarui wa-gateway-autodeploy (timer systemd) di server produksi.
#
#   sudo scripts/install-autodeploy.sh [DEPLOY_DIR]
#   # atau dari Mac, tanpa menyalin repo:
#   AUTODEPLOY_B64=$(base64 < scripts/autodeploy.sh | tr -d '\n') \
#     ssh muhyiddin@10.33.33.5 "sudo env AUTODEPLOY_B64=$AUTODEPLOY_B64 bash -s" \
#     < scripts/install-autodeploy.sh
#
# Butuh scripts/autodeploy.sh di folder yang sama, ATAU env AUTODEPLOY_B64
# (isi skrip dalam base64) bila dijalankan lewat stdin ssh — lihat docs/ci-cd.md §6.
#
# Hasil: /usr/local/bin/wa-gateway-autodeploy, unit wa-gateway-autodeploy.service + .timer
# (tiap 60 detik, mulai 2 menit setelah boot). Log: journalctl -u wa-gateway-autodeploy
# atau /var/log/wa-gateway-autodeploy.log.
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "jalankan sebagai root (sudo)" >&2; exit 1; }

DEPLOY_DIR="${1:-${DEPLOY_DIR:-/home/muhyiddin/docker/wa-gateway}}"
[[ -f "$DEPLOY_DIR/docker-compose.yml" ]] || { echo "docker-compose.yml tidak ada di $DEPLOY_DIR" >&2; exit 1; }
# Service berjalan sebagai pemilik folder (bukan root) agar memakai login GHCR
# di ~/.docker/config.json miliknya; user itu harus anggota grup docker.
RUN_USER="${RUN_USER:-$(stat -c %U "$DEPLOY_DIR")}"
id -nG "$RUN_USER" | tr ' ' '\n' | grep -qx docker || { echo "user $RUN_USER bukan anggota grup docker" >&2; exit 1; }

BIN=/usr/local/bin/wa-gateway-autodeploy
HERE="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || true)"
if [[ -n "${AUTODEPLOY_B64:-}" ]]; then
  printf '%s' "$AUTODEPLOY_B64" | base64 -d > "$BIN"
elif [[ -n "$HERE" && -f "$HERE/autodeploy.sh" ]]; then
  install -m 0755 "$HERE/autodeploy.sh" "$BIN"
else
  echo "autodeploy.sh tidak ditemukan (set AUTODEPLOY_B64 bila via stdin)" >&2; exit 1
fi
chmod 0755 "$BIN"
bash -n "$BIN"

cat > /etc/systemd/system/wa-gateway-autodeploy.service <<EOF
[Unit]
Description=WA Gateway — pull-based auto deploy (${DEPLOY_DIR})
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=${RUN_USER}
Environment=DEPLOY_DIR=${DEPLOY_DIR}
Environment="SERVICES=wa-gateway"
Environment=LOG_FILE=/var/log/wa-gateway-autodeploy.log
Environment=LOCK_FILE=/tmp/wa-gateway-autodeploy.lock
ExecStart=${BIN}
TimeoutStartSec=20min
EOF

cat > /etc/systemd/system/wa-gateway-autodeploy.timer <<'EOF'
[Unit]
Description=Jalankan wa-gateway-autodeploy tiap menit

[Timer]
OnBootSec=2min
OnUnitActiveSec=60s
AccuracySec=10s
Unit=wa-gateway-autodeploy.service

[Install]
WantedBy=timers.target
EOF

touch /var/log/wa-gateway-autodeploy.log
chown "$RUN_USER" /var/log/wa-gateway-autodeploy.log
systemctl daemon-reload
systemctl enable --now wa-gateway-autodeploy.timer
systemctl list-timers wa-gateway-autodeploy.timer --no-pager
echo "✓ wa-gateway-autodeploy terpasang — DEPLOY_DIR=${DEPLOY_DIR}, user=${RUN_USER}"
echo "  log: journalctl -u wa-gateway-autodeploy -f   |   tail -f /var/log/wa-gateway-autodeploy.log"
echo "  jalankan sekarang: systemctl start wa-gateway-autodeploy.service"
