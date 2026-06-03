# Copilot Instructions — wa-gateway

WhatsApp Gateway microservice ditulis dalam **Go** menggunakan library
[`whatsmeow`](https://go.mau.fi/whatsmeow). Mengekspos REST API untuk kebutuhan
**notifikasi, OTP, dan AI tutor**. Service berjalan standalone (Pola A) dan
mendukung multi-session (banyak nomor WhatsApp dalam satu proses).

## Arsitektur

```
main.go                       # entrypoint: config → Manager → API server, graceful shutdown
internal/
  config/config.go            # loader env var (semua setting)
  gateway/
    manager.go                # orkestrasi multi-session, owns db/container/notifier/store/bulk
    session.go                # per-nomor: connect, QR, Send{Text,Image,File,Voice}, parseJID, ListGroups
    webhook.go                # webhookNotifier: queue + worker pool + retry/backoff
    store.go                  # persistensi pesan (tabel gw_messages)
    bulk.go                   # bulk sender async: renderTemplate, job, jitter delay
    session_test.go           # unit test parseJID
    bulk_test.go              # unit test renderTemplate
  api/server.go               # REST routes + auth middleware + handlers
```

Alur dependency: `Config` → `Manager` (memegang DB, container whatsmeow, notifier,
store, bulk runner) → `Session` per nomor. API server hanya memanggil `Manager`.

## Konvensi & aturan penting

- **Bahasa**: dokumentasi & pesan commit dalam **Bahasa Indonesia**. Komentar kode
  ringkas dan hanya bila perlu.
- **Go version**: 1.26.x. Selalu jalankan `gofmt` agar tidak ada diff format.
- **CGO disabled**: build dengan `CGO_ENABLED=0`. SQLite memakai driver pure-Go
  `modernc.org/sqlite` — **jangan** ganti ke driver berbasis CGO (`mattn/go-sqlite3`).
- **SQLite DSN** wajib mengaktifkan foreign keys, jika tidak `Upgrade()` gagal:
  `file:<path>?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)`, dan
  `db.SetMaxOpenConns(1)`. Buka via `sql.Open("sqlite", dsn)` lalu
  `sqlstore.NewWithDB(db, "sqlite3", log)`.
- **Routing**: pakai pattern Go 1.22+ (`mux.HandleFunc("DELETE /sessions/{name}", ...)`
  + `r.PathValue("name")`). Jangan tambah router pihak ketiga.
- **QR flow**: saat `wa.Store.ID == nil`, panggil `GetQRChannel(ctx)` **sebelum**
  `Connect()`, lalu konsumsi di goroutine.
- **parseJID**: normalisasi nomor lokal `0...` ke format internasional memakai
  `DEFAULT_COUNTRY_CODE`. Tolak nomor leading-0 tanpa country code segera (cegah hang 60s).
  Group JID memakai suffix `@g.us` dan sudah diterima semua endpoint `/send/*`.
- **Persistensi**: `data/` berisi kredensial WhatsApp — **JANGAN PERNAH** di-commit.
  Sudah di-`.gitignore` bersama `*.db*`, `.env`, `*.png`, biner.
- **Kredensial/secret**: jangan hardcode. Semua via env var (lihat `config.go` / `.env.example`).

## Validasi sebelum selesai

Selalu jalankan dan pastikan bersih/lulus:

```bash
go build ./...
go vet ./...
go test ./...
gofmt -l .            # harus tanpa output
docker compose config # bila menyentuh compose/Dockerfile
```

## Dokumentasi API

- **OpenAPI 3.0 spec:** [`openapi.yaml`](../openapi.yaml) — machine-readable, gunakan
  untuk code generation atau referensi lengkap schema request/response.
- **Markdown untuk project consumer:** [`docs/copilot-api.md`](../docs/copilot-api.md) —
  snippet siap-tempel ke `.github/copilot-instructions.md` project lain beserta contoh
  integrasi TypeScript, Python, dan Go.

## REST API (lihat `internal/api/server.go`)

Semua route kecuali `/health` dilindungi middleware `auth` (header
`X-API-Key` / `Authorization: Bearer`, aktif bila `API_KEY` di-set).

- `GET  /health` — liveness (tanpa auth)
- `GET  /status` — status koneksi session
- `GET  /qr` — ambil QR untuk pairing
- `POST /pair` — pairing via kode (alternatif QR, `{"phone":"628..."}`)
- `GET  /groups` — daftar group yang diikuti
- `GET  /messages` — riwayat pesan (filter: session, chat, limit, before)
- `GET|POST /sessions`, `DELETE /sessions/{name}` — CRUD session
- `POST /send/text|image|file|voice` — kirim pesan
- `POST /send/bulk`, `GET /send/bulk`, `GET /send/bulk/{id}` — bulk async + progress
- `POST /logout`

### Bulk send & template

`POST /send/bulk` async — mengembalikan `jobId` segera. Pesan personal memakai
template + vars per penerima: `"template": "Halo {{name}}, nilai {{nilai}}"` dengan
`"vars": {"name": "Budi", "nilai": "90"}`. Prioritas isi pesan:
`messages[].text` > `render(template, vars)` > `text` global. Antar pengiriman ada
jitter delay (`BULK_MIN_DELAY_MS`/`BULK_MAX_DELAY_MS`).

## Konfigurasi (env var — `internal/config/config.go`)

Inti: `PORT`, `API_KEY`, `STORE_DIR`, `LOG_LEVEL`, `DEFAULT_COUNTRY_CODE`.
Webhook: `WEBHOOK_URL`, `WEBHOOK_EVENTS`, `WEBHOOK_WORKERS`, `WEBHOOK_QUEUE_SIZE`,
`WEBHOOK_MAX_RETRIES`, `WEBHOOK_BACKOFF_MS`, `DOWNLOAD_MEDIA`, `MAX_DOWNLOAD_BYTES`.
Storage: `STORE_MESSAGES`, `MESSAGE_RETENTION_DAYS`.
Bulk: `BULK_MIN_DELAY_MS`, `BULK_MAX_DELAY_MS`.
Tambah setting baru → daftarkan di struct `Config`, `Load()`, `.env.example`, dan README.

## Catatan implementasi

- Webhook hanya aktif jika `WEBHOOK_URL` di-set; meneruskan semua pesan termasuk
  group (`isGroup:true`, `sender`, `from`). `DOWNLOAD_MEDIA=true` melampirkan media base64.
- Tabel pesan `gw_messages` memakai `INSERT OR IGNORE` (dedup by session+id); hanya
  metadata media yang disimpan, bukan isi file. Retention purge jalan saat start + harian.
- Tabel session kustom `gw_sessions(name TEXT PRIMARY KEY, jid TEXT)`; pada event
  `PairSuccess`, `bindJID()` meng-update jid.
- `renderTemplate`: regex `\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`; placeholder tak dikenal
  dibiarkan apa adanya.
