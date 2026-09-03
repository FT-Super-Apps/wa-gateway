# Copilot Instructions — wa-gateway

WhatsApp Gateway microservice ditulis dalam **Go** menggunakan library
[`whatsmeow`](https://go.mau.fi/whatsmeow). Mengekspos REST API untuk kebutuhan
**notifikasi, OTP, dan AI tutor**. Service berjalan standalone (Pola A) dan
mendukung multi-session (banyak nomor WhatsApp dalam satu proses).

## Arsitektur

```
main.go                       # entrypoint: config → Manager → API server, graceful shutdown
cmd/wagctl/main.go            # CLI: keys (create/list/get/update/enable/disable/rotate/delete), status, qr, pair, check, normalize, send
internal/
  config/config.go            # loader env var (semua setting)
  gateway/
    manager.go                # orkestrasi multi-session, owns db/container/notifier/store/bulk/keys/accesslog
    session.go                # per-nomor: connect, QR, pair, Send{Text,Image,File,Voice} (+quote/replyTo), parseJID, ListGroups, CheckPhones, ResolveLIDs
    db.go                     # pgDB: wrapper *sql.DB yang mengubah placeholder ? → $n
    webhook.go                # webhookNotifier: queue + worker pool + retry/backoff; payload message/receipt
    store.go                  # persistensi pesan (tabel gw_messages) + status kirim
    storefilter.go            # filter chat yang disimpan (STORE_CHATS / STORE_CHATS_EXCLUDE)
    mediastore.go             # MediaStore: byte media ke disk atau S3/MinIO
    bulk.go                   # bulk sender async: renderTemplate, job, jitter delay, auto-resume (gw_bulk_jobs/gw_bulk_messages)
    apikey.go                 # managed API key: scope, rate limit, expiry, rotate (gw_api_keys)
    accesslog.go              # access log per request terautentikasi (gw_access_logs)
    *_test.go                 # unit test: parseJID, renderTemplate, bulkstore, apikey, receipt, messagestatus, db
  api/server.go               # REST routes + auth middleware (scope + rate limit + access log) + handlers
```

Alur dependency: `Config` → `Manager` (memegang DB, container whatsmeow, notifier,
store, media store, bulk runner, key store, access log) → `Session` per nomor.
API server hanya memanggil `Manager`.

## Konvensi & aturan penting

- **Bahasa**: dokumentasi & pesan commit dalam **Bahasa Indonesia**. Komentar kode
  ringkas dan hanya bila perlu.
- **Go version**: 1.26.x. Selalu jalankan `gofmt` agar tidak ada diff format.
- **CGO disabled**: build dengan `CGO_ENABLED=0`. Driver database memakai pure-Go
  `github.com/jackc/pgx/v5/stdlib` — **jangan** ganti ke driver berbasis CGO.
- **PostgreSQL**: koneksi via `DATABASE_URL` (wajib). Buka dengan
  `sql.Open("pgx", cfg.DatabaseURL)`, pool normal (`SetMaxOpenConns(20)`), lalu
  `sqlstore.NewWithDB(db, "postgres", log)`. Semua store dibungkus `pgDB`
  ([db.go](../internal/gateway/db.go)) yang otomatis mengubah placeholder `?`
  menjadi `$n` — tulis SQL dengan `?` seperti biasa. Upsert pakai
  `ON CONFLICT ... DO NOTHING`; migrasi kolom pakai `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`.
- **Routing**: pakai pattern Go 1.22+ (`mux.HandleFunc("DELETE /sessions/{name}", ...)`
  + `r.PathValue("name")`). Jangan tambah router pihak ketiga.
- **QR flow**: saat `wa.Store.ID == nil`, panggil `GetQRChannel(ctx)` **sebelum**
  `Connect()`, lalu konsumsi di goroutine.
- **parseJID**: normalisasi nomor lokal `0...` ke format internasional memakai
  `DEFAULT_COUNTRY_CODE`. Tolak nomor leading-0 tanpa country code segera (cegah hang 60s).
  JID dengan suffix `@g.us`, `@s.whatsapp.net`, `@newsletter`, `@lid` diteruskan apa adanya;
  `@c.us` dinormalisasi. Group JID sudah diterima semua endpoint `/send/*`.
- **Reply/quote**: semua `/send/*` menerima `replyTo {id,text,fromMe}` → `gateway.QuoteRef`
  → `ContextInfo` (StanzaID/Participant/QuotedMessage). Pesan masuk yang berupa balasan
  membawa `replyToId`/`replyToText` di webhook (`quotedInfo`).
- **LID**: `events.Message` bisa datang dengan `sender` `@lid`; webhook menyertakan
  `senderAlt` (`Info.SenderAlt`). `POST /resolve-lid` memakai `Store.LIDs.GetPNForLID`.
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
- **Panduan integrasi developer consumer:** [`docs/integration-guide.md`](../docs/integration-guide.md) —
  alur end-to-end (API key, OTP, bulk, chatbot, kontrak webhook, error/rate limit, checklist go-live).

## REST API (lihat `internal/api/server.go`)

Semua route kecuali `/health` dilindungi middleware `auth(scope, handler)`: header
`X-API-Key` / `Authorization: Bearer`, aktif bila `API_KEY` di-set **atau** ada managed key.
Scope: `read`, `send`, `sessions`, `admin`, `*` (master key = semua). Rate limit fixed-window
→ `429` + `X-RateLimit-*` + `Retry-After`. Tiap request lolos auth dicatat ke access log.

- `GET  /health` — liveness (tanpa auth)
- `GET  /status` — status koneksi session (`read`)
- `GET  /qr` — ambil QR untuk pairing (`read`)
- `POST /pair` — pairing via kode (alternatif QR, `{"phone":"628..."}`) (`sessions`)
- `GET  /groups` — daftar group yang diikuti (`read`)
- `GET  /messages` — riwayat pesan (filter: session, chat, limit, before, since, order=asc) (`read`)
- `POST /messages/status` — status kirim (`sent`/`delivered`/`read`/`played`) banyak messageId sekaligus; alternatif pull untuk webhook `receipt` (`read`)
- `GET  /messages/{id}/media` — unduh byte media tersimpan (butuh `STORE_MEDIA=true`) (`read`)
- `GET|POST /sessions`, `DELETE /sessions/{name}` — CRUD session (`read` / `sessions`)
- `POST /send/text|image|file|voice` — kirim pesan, opsional `replyTo` (`send`)
- `POST /send/bulk`, `GET /send/bulk`, `GET /send/bulk/{id}` — bulk async + progress (`send` / `read`)
- `POST /normalize` — normalisasi nomor (tanpa session) (`send`)
- `POST /check` — cek nomor terdaftar di WA, maks 250 (`send`)
- `POST /resolve-lid` — map JID `@lid` → nomor, maks 500 (`send`)
- `POST /logout` (`sessions`)
- `POST|GET /admin/keys`, `GET|PATCH|DELETE /admin/keys/{id}`, `POST /admin/keys/{id}/{rotate,enable,disable}` — managed key (`admin`)
- `GET /admin/logs`, `GET /admin/keys/{id}/logs` — access log (filter: key, since, limit) (`admin`)

### Bulk send & template

`POST /send/bulk` async — mengembalikan `jobId` segera. Pesan personal memakai
template + vars per penerima: `"template": "Halo {{name}}, nilai {{nilai}}"` dengan
`"vars": {"name": "Budi", "nilai": "90"}`. Prioritas isi pesan:
`messages[].text` > `render(template, vars)` > `text` global. Antar pengiriman ada
jitter delay (`BULK_MIN_DELAY_MS`/`BULK_MAX_DELAY_MS`).

## Konfigurasi (env var — `internal/config/config.go`)

Inti: `PORT`, `API_KEY`, `DATABASE_URL`, `STORE_DIR`, `LOG_LEVEL`, `DEFAULT_COUNTRY_CODE`.
Webhook: `WEBHOOK_URL`, `WEBHOOK_EVENTS`, `WEBHOOK_WORKERS`, `WEBHOOK_QUEUE_SIZE`,
`WEBHOOK_MAX_RETRIES`, `WEBHOOK_BACKOFF_MS`, `DOWNLOAD_MEDIA`, `MAX_DOWNLOAD_BYTES`.
Storage: `STORE_MESSAGES`, `MESSAGE_RETENTION_DAYS`, `STORE_CHATS`, `STORE_CHATS_EXCLUDE`,
`STORE_MEDIA`, `MEDIA_BACKEND` (`disk`|`s3`), `MEDIA_DIR`, `S3_ENDPOINT`, `S3_BUCKET`,
`S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_USE_SSL`, `S3_REGION`.
Bulk: `BULK_MIN_DELAY_MS`, `BULK_MAX_DELAY_MS`, `BULK_AUTO_RESUME`.
API key: `DEFAULT_RATE_LIMIT`, `DEFAULT_RATE_WINDOW_SEC`, `DEFAULT_MAX_SESSIONS`.
Access log: `ACCESS_LOG_RETENTION_DAYS` (0 = nonaktif).
Tambah setting baru → daftarkan di struct `Config`, `Load()`, `.env.example`, `docker-compose*.yml`,
README, dan `docs/copilot-api.md`.

## Catatan implementasi

- Webhook hanya aktif jika `WEBHOOK_URL` di-set; meneruskan semua pesan termasuk
  group (`isGroup:true`, `sender`, `from`). `DOWNLOAD_MEDIA=true` melampirkan media base64.
- Read receipt: `handleEvent` menangani `events.Receipt` → `handleReceipt` mengirim
  webhook `event:"receipt"` (bila `WEBHOOK_EVENTS` memuat `receipt`) dan memajukan
  status durable pesan keluar via `store.updateStatus` (hanya maju: sent→delivered→read→played,
  pakai `statusRank`). Tipe `-self` (read-self/played-self) diteruskan ke webhook tapi
  tidak mengubah status keluar. `receiptStatus` (webhook.go) memetakan `types.ReceiptType`.
  Kolom `gw_messages`: `status`, `status_ts`; muncul sebagai `status`/`statusAt` di `GET /messages`
  dan `POST /messages/status` (lookup batch per messageId, urut sesuai permintaan).
- Tabel pesan `gw_messages` memakai `INSERT ... ON CONFLICT (session,id) DO NOTHING`
  (dedup by session+id). Kolom media (`mimetype`, `filename`, `file_length`,
  `media_path`) diisi bila `STORE_MEDIA=true`; byte file disimpan lewat `MediaStore`
  ([mediastore.go](../internal/gateway/mediastore.go), backend `disk` atau `s3`/MinIO
  via `MEDIA_BACKEND`), bukan di DB.
  Media masuk diunduh async (goroutine) agar tak memblok event loop; media keluar
  ditulis dari `MediaInput.Data`. Ambil file via `GET /messages/{id}/media`.
  Filter chat opsional (`STORE_CHATS`/`STORE_CHATS_EXCLUDE`) via
  [storefilter.go](../internal/gateway/storefilter.go). `GET /messages` mendukung
  `before` (DESC) dan `since`+`order=asc` (catch-up untuk konsumer offline).
  Retention purge (pesan + file media) jalan saat start + harian.
- Tabel session kustom `gw_sessions(name TEXT PRIMARY KEY, jid TEXT, owner_key TEXT)`; pada event
  `PairSuccess`, `bindJID()` meng-update jid. `owner_key` = id managed key pembuat (untuk `maxSessions`).
- Managed key (`gw_api_keys`): secret `wag_` + 40 hex, disimpan hash SHA-256; plaintext hanya
  saat create/rotate. Master key = `API_KEY` env (scope semua, tanpa limit). Rate limit
  fixed-window in-memory per key.
- Access log (`gw_access_logs`): buffer in-memory, flush tiap 5s + saat query; purge harian.
  401/403 (invalid, disabled, expired, scope) **tidak** dicatat; 429 dicatat.
- `renderTemplate`: regex `\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`; placeholder tak dikenal
  dibiarkan apa adanya.
- Bulk job persist di `gw_bulk_jobs`/`gw_bulk_messages`; status job `running|completed|cancelled|interrupted`,
  per penerima `pending|sent|failed`. Saat start, `resumeInterrupted` melanjutkan penerima `pending`
  bila `BULK_AUTO_RESUME=true`.

## Saat mengubah API

Setiap perubahan route/field request/response **wajib** diikuti update di: `openapi.yaml`,
`README.md`, `docs/copilot-api.md`, dan bagian REST API di file ini. Bila mengubah perilaku yang
dilihat consumer (webhook, error code, alur kirim), update juga `docs/integration-guide.md`.
Bila menyentuh access log, update juga `docs/access-log.md`.
