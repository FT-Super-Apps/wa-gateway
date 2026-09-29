# Changelog

Semua perubahan penting pada proyek WA Gateway didokumentasikan di file ini.

Format mengikuti [Keep a Changelog](https://keepachangelog.com/id-ID/1.1.0/) dan penomoran [Semantic Versioning](https://semver.org/lang/id/).

**Konvensi versi & build** (lihat [docs/ci-cd.md](docs/ci-cd.md) § Versioning):
- Nomor versi tunggal ada di file [VERSION](VERSION). Binary, image Docker, dan tag git membaca dari sumber yang sama.
- Setiap deploy lewat GitHub Actions (`Build & Deploy`) **menaikkan versi otomatis** (default `patch`; pilih `minor`/`major` saat menjalankan workflow). Job `release` menjalankan `scripts/bump-version.sh`, yang mengubah heading `## [Unreleased]` di bawah menjadi `## [X.Y.Z] — tanggal (build N)`, lalu membuat commit `chore(release): vX.Y.Z`, tag `vX.Y.Z`, dan GitHub Release.
- **Nomor build** = `github.run_number`; ikut dibakar ke binary (`GET /version`, header `X-API-Version`) dan label image (`org.opencontainers.image.version`, `id.ac.unismuh.lms.build-number`).
- Tulis perubahan baru **hanya** di bawah `## [Unreleased]` — jangan menambah tanggal pada heading itu; skrip yang akan menamainya saat rilis.
- Pilih `bump`: patch = bug fix/docs/upgrade dependensi; minor = endpoint/fitur baru yang kompatibel; major = perubahan kontrak API/skema auth yang memaksa perubahan di aplikasi pemakai (LMS, CRM).

---

## [Unreleased]

### Fixed
- **Upgrade `go.mau.fi/whatsmeow`** ke rilis 2026-09-28 (`35f522c`) — memperbaiki **`Client outdated (405) connect failure`** (client version `2.3000.1040390703` ditolak WhatsApp sejak 28 Sep 2026). Gejala di aplikasi pemakai: `/send/text` 502 `websocket not connected`, `/groups/*` 409 `session is not logged in`, `GET /status` → `connected:false`. Sesi tersimpan tetap dipakai — tidak perlu pairing ulang.

### Added
- **Sistem versi & rilis identik dengan LMS OBE AI**: file `VERSION` (sumber tunggal), `pkg/version` (di-stamp via `-ldflags` dari `scripts/version-ldflags.sh` / build-arg Dockerfile), `GET /version` (**tanpa auth**, bentuk `{version,commit,build_time,build_number,go_version}`), header `X-API-Version` di semua respons, field `version/build_number/commit` di `GET /health`, log startup `wa-gateway vX.Y.Z (build N, sha)`.
- Workflow `Build & Deploy` versi baru: job `release` (bump `VERSION` + potong CHANGELOG + commit `chore(release)` + tag + GitHub Release) → `test` → `build` (GHCR `:X.Y.Z`, `:build-N`, `:latest`; `skip_deploy` tanpa `:latest`) → `deploy` **pull-based** (server menarik `:latest` lewat timer `wa-gateway-autodeploy`; job hanya menunggu `GET /version` bila `vars.PUBLIC_URL` diset). Concurrency `deploy-production`.
- Skrip: `scripts/bump-version.sh` (salinan verbatim LMS), `scripts/version-ldflags.sh`, `scripts/release.sh` (cek lokal → push → trigger → pantau), `scripts/autodeploy.sh` + `scripts/install-autodeploy.sh` (unit systemd `wa-gateway-autodeploy.{service,timer}`), `scripts/check-mermaid.mjs`.
- Image: label OCI (`org.opencontainers.image.version/revision/created/source`) + `id.ac.unismuh.lms.build-number`; env `APP_VERSION/APP_COMMIT/APP_BUILD_NUMBER`. `docker-compose.prod.yml` memakai `${IMAGE_TAG:-latest}` untuk rollback.

### Changed
- Deploy tidak lagi lewat SSH/SCP dari runner GitHub (jalur runner → jump host → LAN kampus sering putus); folder prod di server bersifat *hand-maintained* dan tidak pernah ditimpa repo.

### Documentation
- `docs/ci-cd.md` — arsitektur pipeline, deploy pull-based, konfigurasi GitHub, setup server, rollback, troubleshooting, versioning.

---

## [1.0.9] — 2026-08-26

### Added
- `POST /messages/status` — status pengiriman (`sent/delivered/read/played`) untuk banyak `messageId` sekaligus.

## [1.0.8] — 2026-07-28

### Added
- Webhook mengenali pesan undangan grup (`groupInviteMessage`).

## [1.0.7] — 2026-07-24

### Added
- Reply/quote asli WhatsApp (`replyTo`) pada pengiriman; reply inbound diteruskan di webhook.

## [1.0.6] — 2026-07-24

### Added
- `senderAlt` di webhook untuk resolusi LID, endpoint `POST /resolve-lid`, kirim ke alamat `@lid`.

## [1.0.5] — 2026-07-20

### Added
- Pelacakan tanda terima (delivered/read) per pesan dan pembaruan status di penyimpanan.

## [1.0.4] — 2026-07-18

### Fixed
- Simpan mimetype media keluar agar ekstensi & `Content-Type` benar.

## [1.0.3] — 2026-07-18

### Added
- Service MinIO di `docker-compose` + hardening inisialisasi S3.

## [1.0.2] — 2026-07-18

### Added
- Backend media MinIO/S3 (`MEDIA_BACKEND=s3`).

## [1.0.1] — 2026-07-18

### Changed
- `wagctl` fallback ke `PORT` & `API_KEY` container agar bisa dijalankan tanpa set env.

## [1.0.0] — 2026-07-17

### Changed
- Migrasi penyimpanan sesi & pesan ke PostgreSQL (pgx, tanpa CGO); penyimpanan media & filter chat.

### Added
- Fitur dasar sejak 2026-06-03: REST kirim teks/gambar/file/voice, webhook + worker pool, bulk send dengan template & auto-resume, pairing QR/kode, multi-session, API key management (scope, rate limit, expiry, rotate), access log, normalisasi & cek nomor, CLI `wagctl`, OpenAPI spec.
