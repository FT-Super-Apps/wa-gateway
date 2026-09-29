#!/usr/bin/env bash
# Satu perintah untuk: cek lokal → commit (opsional) → push → trigger "Build & Deploy"
# → pantau run → verifikasi versi yang berjalan di server.
#
#   scripts/release.sh [patch|minor|major|none] [opsi]
#
# Opsi:
#   -m, --message "pesan"  commit semua perubahan working tree dengan pesan ini sebelum push
#   -y, --yes              jangan tanya konfirmasi (non-interaktif)
#       --no-check         lewati pemeriksaan lokal (go build/vet, gofmt)
#       --full             pemeriksaan lokal setara CI penuh: + go test -race
#       --skip-deploy      hanya release + build + push image (input skip_deploy=true)
#       --no-watch         trigger saja, jangan pantau run
#   -h, --help
#
# Contoh:
#   scripts/release.sh                       # bump patch, tanya konfirmasi
#   scripts/release.sh minor -m "feat: ..."  # commit dulu, lalu rilis minor
#   scripts/release.sh none -y               # redeploy versi VERSION apa adanya
#
# Prasyarat: git, gh (sudah `gh auth login`), go. Harus di branch main
# dan tidak tertinggal dari origin/main (CI bump VERSION di atas HEAD origin).
# Gateway berjalan di LAN tanpa URL publik: set PROD_URL (mis. lewat SSH tunnel)
# bila ingin skrip memverifikasi versi yang berjalan; kosong = dilewati.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WORKFLOW="deploy.yml"
BRANCH="main"
PROD_URL="${PROD_URL:-}"

bump="patch"
msg=""
yes=0
check=1
full=0
skip_deploy=0
watch=1

usage() { sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-1}"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    patch|minor|major|none) bump="$1"; shift ;;
    -m|--message) msg="$2"; shift 2 ;;
    -y|--yes) yes=1; shift ;;
    --no-check) check=0; shift ;;
    --full) full=1; shift ;;
    --skip-deploy) skip_deploy=1; shift ;;
    --no-watch) watch=0; shift ;;
    -h|--help) usage 0 ;;
    *) echo "opsi tidak dikenal: $1" >&2; usage ;;
  esac
done

# ── util ──────────────────────────────────────────────────────────────────────
c_bold=$'\033[1m'; c_dim=$'\033[2m'; c_red=$'\033[31m'; c_grn=$'\033[32m'; c_ylw=$'\033[33m'; c_rst=$'\033[0m'
step() { printf '\n%s▶ %s%s\n' "$c_bold" "$*" "$c_rst"; }
ok()   { printf '%s✓ %s%s\n' "$c_grn" "$*" "$c_rst"; }
warn() { printf '%s⚠ %s%s\n' "$c_ylw" "$*" "$c_rst"; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_rst" >&2; exit 1; }
confirm() {
  [[ $yes -eq 1 ]] && return 0
  local ans
  read -r -p "$1 [y/N] " ans
  [[ "$ans" =~ ^[Yy]$ ]]
}
need() { command -v "$1" >/dev/null 2>&1 || die "perintah '$1' tidak ditemukan"; }

# ── 0. prasyarat ──────────────────────────────────────────────────────────────
step "Prasyarat"
need git; need gh; need curl
gh auth status >/dev/null 2>&1 || die "gh belum login — jalankan: gh auth login"
cur_branch="$(git rev-parse --abbrev-ref HEAD)"
[[ "$cur_branch" == "$BRANCH" ]] || die "harus di branch $BRANCH (sekarang: $cur_branch)"
ok "gh terautentikasi, branch $BRANCH"

# ── 1. working tree ───────────────────────────────────────────────────────────
step "Working tree"
if [[ -n "$(git status --porcelain)" ]]; then
  git status --short
  if [[ -n "$msg" ]]; then
    confirm "Commit SEMUA perubahan di atas dengan pesan \"$msg\"?" || die "dibatalkan"
    git add -A
    git commit -q -m "$msg"
    ok "commit $(git rev-parse --short HEAD)"
  else
    die "working tree kotor — commit dulu, atau beri -m \"pesan\" agar skrip yang commit"
  fi
else
  ok "bersih"
fi

git fetch -q origin "$BRANCH"
behind="$(git rev-list --count HEAD..origin/"$BRANCH")"
ahead="$(git rev-list --count origin/"$BRANCH"..HEAD)"
if [[ "$behind" -gt 0 ]]; then
  die "lokal tertinggal $behind commit dari origin/$BRANCH — jalankan: git pull --rebase origin $BRANCH"
fi
ok "sinkron dengan origin ($ahead commit belum dipush)"

# ── 2. CHANGELOG ──────────────────────────────────────────────────────────────
if [[ "$bump" != "none" ]]; then
  step "CHANGELOG [Unreleased]"
  unreleased="$(awk '/^## \[Unreleased\]/{f=1;next} f&&/^## /{exit} f' CHANGELOG.md | grep -vE '^\s*$|^---$|_Belum ada perubahan yang belum dirilis\._' || true)"
  if [[ -z "$unreleased" ]]; then
    warn "bagian [Unreleased] kosong — rilis akan tercatat sebagai 'Rilis pemeliharaan'"
    confirm "Lanjut tanpa catatan perubahan?" || die "isi CHANGELOG.md di bawah '## [Unreleased]' dulu"
  else
    printf '%s\n' "$unreleased" | head -12 | sed "s/^/${c_dim}  /; s/\$/${c_rst}/"
    [[ "$(printf '%s\n' "$unreleased" | wc -l)" -gt 12 ]] && printf '%s  …%s\n' "$c_dim" "$c_rst"
    ok "ada catatan perubahan"
  fi
fi

# ── 3. pemeriksaan lokal (cermin ci.yml) ──────────────────────────────────────
if [[ $check -eq 1 ]]; then
  step "Go: gofmt + go build + go vet (seperti CI)"
  need go
  unfmt="$(gofmt -l . 2>/dev/null || true)"
  [[ -z "$unfmt" ]] || die "belum gofmt: $unfmt"
  ( go build ./... && go vet ./... ) || die "go build/vet gagal"
  ok "go build + vet"
  if [[ $full -eq 1 ]]; then
    step "Go: go test -race"
    go test ./... -race || die "go test gagal"
    ok "go test"
  fi

  if [[ -f scripts/check-mermaid.mjs ]]; then
    step "Docs: validasi Mermaid"
    node scripts/check-mermaid.mjs docs README.md >/dev/null || die "diagram Mermaid tidak valid"
    ok "mermaid"
  fi
else
  warn "pemeriksaan lokal dilewati (--no-check)"
fi

# ── 4. push ───────────────────────────────────────────────────────────────────
step "Push ke origin/$BRANCH"
if [[ "$ahead" -gt 0 ]]; then
  git push -q origin "$BRANCH"
  ok "pushed $(git rev-parse --short HEAD)"
else
  ok "tidak ada commit baru untuk dipush"
fi

# ── 5. trigger workflow ───────────────────────────────────────────────────────
current_ver="$(tr -d '[:space:]' < VERSION)"
if [[ "$bump" == "none" ]]; then
  expect_ver="$current_ver"
else
  expect_ver="$(scripts/bump-version.sh "$bump" --dry-run 2>/dev/null)"
fi
step "Trigger '$WORKFLOW' — bump=$bump (v$current_ver → v$expect_ver)$( [[ $skip_deploy -eq 1 ]] && echo ', skip_deploy' )"
confirm "Jalankan rilis & deploy ke produksi sekarang?" || die "dibatalkan"

dispatch_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
args=(-f "bump=$bump")
[[ $skip_deploy -eq 1 ]] && args+=(-f "skip_deploy=true")
gh workflow run "$WORKFLOW" --ref "$BRANCH" "${args[@]}"

# Tunggu run yang dibuat setelah dispatch muncul di daftar.
run_id=""
for _ in $(seq 1 20); do
  sleep 3
  run_id="$(gh run list --workflow="$WORKFLOW" --event=workflow_dispatch --branch="$BRANCH" --limit=5 \
    --json databaseId,createdAt --jq "[.[] | select(.createdAt >= \"$dispatch_at\")] | sort_by(.createdAt) | last | .databaseId // empty" 2>/dev/null || true)"
  [[ -n "$run_id" ]] && break
done
[[ -n "$run_id" ]] || die "run tidak ditemukan — cek: gh run list --workflow=$WORKFLOW"
run_url="$(gh run view "$run_id" --json url --jq .url)"
ok "run #$run_id → $run_url"

if [[ $watch -eq 0 ]]; then
  echo "Pantau manual: gh run watch $run_id"
  exit 0
fi

# ── 6. pantau ─────────────────────────────────────────────────────────────────
step "Memantau run (release → test → build → deploy)"
if ! gh run watch "$run_id" --exit-status --interval 20; then
  printf '\n%sLog job yang gagal:%s\n' "$c_bold" "$c_rst"
  gh run view "$run_id" --log-failed 2>/dev/null | tail -60 || true
  die "run #$run_id GAGAL — $run_url"
fi
ok "run selesai"

# ── 7. verifikasi versi & tarik commit rilis ──────────────────────────────────
if [[ $skip_deploy -eq 0 && -n "$PROD_URL" ]]; then
  step "Verifikasi versi di $PROD_URL"
  running="$(curl -sf --max-time 15 "${PROD_URL%/}/version" || echo '{}')"
  run_ver="$(printf '%s' "$running" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
  echo "  $running"
  if [[ "$run_ver" == "$expect_ver" ]]; then
    ok "v$run_ver aktif di produksi"
  else
    warn "versi berjalan ($run_ver) ≠ yang diharapkan ($expect_ver) — cek log deploy"
  fi
elif [[ $skip_deploy -eq 0 ]]; then
  warn "PROD_URL kosong — verifikasi di server: curl -s localhost:8111/version  |  journalctl -u wa-gateway-autodeploy -n 20"
fi

if [[ "$bump" != "none" ]]; then
  step "Tarik commit rilis & tag"
  git pull -q --rebase origin "$BRANCH" --tags
  ok "lokal di $(git rev-parse --short HEAD) — $(git describe --tags --abbrev=0 2>/dev/null || echo 'tanpa tag')"
fi

printf '\n%s🎉 Selesai — v%s%s\n' "$c_grn$c_bold" "$expect_ver" "$c_rst"
