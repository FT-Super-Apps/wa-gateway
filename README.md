# WA Gateway

WhatsApp Gateway mandiri (standalone microservice) berbasis **Go + [whatsmeow](https://github.com/tulir/whatsmeow)**.
Mengekspos **REST API** untuk kirim pesan/file dan **webhook** untuk menerima pesan masuk.

Cocok untuk kebutuhan **notifikasi**, **OTP**, dan **AI tutor** — dipakai bersama oleh banyak aplikasi lewat HTTP.

```
┌──────────────┐   HTTP/REST    ┌─────────────────────┐
│  App Utama   │ ─────────────► │   WA Gateway (Go)   │ ──► WhatsApp
│ (bahasa apa  │ ◄── webhook ── │   + whatsmeow       │
│  pun)        │                │   + REST API        │
└──────────────┘                └─────────────────────┘
```

> ⚠️ **Disclaimer:** Ini memakai WhatsApp Web protocol (unofficial). WhatsApp tidak mengizinkan bot/klien tidak resmi, sehingga nomor **berisiko diblokir**. Untuk OTP/produksi kritis, pertimbangkan WhatsApp Cloud API resmi.

## Fitur

- 🔑 Login via **QR code** atau **pairing code** (untuk server headless)
- 💾 **Session persisten** (PostgreSQL, driver pure-Go `pgx`, tanpa CGO) — tidak perlu scan ulang tiap restart
- 👥 **Multi-session** — banyak nomor WhatsApp dalam satu service, di-manage via API
- 📤 Kirim **teks**, **gambar**, **file/dokumen**, dan **voice note** (sumber: URL atau base64)
- 📣 **Bulk send** async dengan template `{{var}}`, jitter delay anti-ban, dan **auto-resume** saat crash/restart
- 📥 Terima pesan masuk via **webhook** (termasuk media sebagai base64)
- 🔁 **Webhook queue** dengan worker pool + retry/backoff eksponensial
- 💬 **Riwayat pesan** opsional ke PostgreSQL (`GET /messages`) + **penyimpanan media** ke disk (`GET /messages/{id}/media`)
- 🔒 **API key management** — banyak key dengan scope, rate limit, batas device, expiry, enable/disable, rotate
- 📊 **Access log monitoring** — catat tiap request terautentikasi (per key) untuk audit
- 🖥️ **CLI `wagctl`** — kelola key, pairing (QR/kode), kirim pesan dari terminal
- 🐳 Siap **Docker** / docker-compose

## Menjalankan

### Lokal (Go)

```bash
cp .env.example .env      # sesuaikan
go run .
```

### Docker Compose

```bash
docker compose up --build -d
docker compose logs -f
```

## CLI (`wagctl`)

`wagctl` adalah tool CLI bawaan untuk mengelola API key dan mengoperasikan gateway
langsung dari terminal — tanpa perlu menulis `curl` manual.

### Build

```bash
go build -o wagctl ./cmd/wagctl/
# atau install ke $GOPATH/bin:
go install ./cmd/wagctl/
```

### Konfigurasi

```bash
export WA_GATEWAY_URL=http://localhost:3111
export WA_GATEWAY_API_KEY=<master-key>  # atau managed key ber-scope admin
```

### Contoh penggunaan

```bash
# Daftar semua API key
wagctl keys list

# Buat key baru (rate limit 100/menit, max 2 session, scope send+read)
wagctl keys create --name="app-otp" --scopes="send,read" \
  --rate-limit=100 --rate-window=60 --max-sessions=2
# ⚠️ Output menampilkan secret sekali — simpan segera!

# Detail satu key
wagctl keys get key_3f1c...

# Nonaktifkan key
wagctl keys disable key_3f1c...
wagctl keys enable key_3f1c...           # aktifkan lagi
wagctl keys update key_3f1c... --enabled=false   # alternatif via update

# Ubah rate limit
wagctl keys update key_3f1c... --rate-limit=200

# Rotate secret (secret lama langsung tidak berlaku)
wagctl keys rotate key_3f1c...

# Hapus key
wagctl keys delete key_3f1c...            # minta konfirmasi
wagctl keys delete key_3f1c... --force    # langsung tanpa konfirmasi

# Cek status semua session
wagctl status

# Pairing WhatsApp: tampilkan QR di terminal (langsung discan)
wagctl qr
wagctl qr --watch                         # render ulang otomatis sampai login
wagctl qr --session="otp" --png=qr.png    # simpan juga sebagai PNG
wagctl qr --raw                           # cetak hanya string kode QR

# Pairing via kode 8-digit (alternatif QR untuk server headless)
wagctl pair --phone="6281122334455"
wagctl pair --phone="6281122334455" --session="otp"

# Cek apakah nomor terdaftar di WhatsApp
wagctl check --phones="6281122334455,628222333444"

# Normalisasi nomor
wagctl normalize --phones="0812-345-678,+6281-234-5678"

# Kirim pesan teks
wagctl send text --to="6281122334455" --text="Halo dari wagctl!"
wagctl send text --to="6281122334455" --text="OTP: 123456" --session="otp"

# Kirim gambar
wagctl send image --to="6281122334455" --url="https://example.com/img.jpg" --caption="Bukti bayar"

# Kirim file
wagctl send file --to="6281122334455" --url="https://example.com/doc.pdf" --filename="laporan.pdf"
```

Tampilkan bantuan tiap subcommand:
```bash
wagctl --help
wagctl keys create --help
wagctl send text --help
```

## Login (scan QR)

Saat service jalan pertama kali, otomatis ada session bernama **`default`**.
Untuk login, ambil QR-nya (parameter `session` opsional, default `default`):

```bash
# Simpan sebagai gambar lalu scan dari WhatsApp > Perangkat tertaut
curl "http://localhost:3000/qr?format=png" -o qr.png

# Untuk session tertentu:
curl "http://localhost:3000/qr?session=otp&format=png" -o qr-otp.png

# atau cek status semua session
curl http://localhost:3000/status
```

Setelah scan berhasil, `loggedIn` pada `/status` menjadi `true`.

## Multi-Session (banyak nomor)

Satu service bisa menjalankan banyak nomor sekaligus (mis. nomor terpisah untuk
**OTP**, **notifikasi**, dan **AI tutor**). Setiap session punya nama unik dan
kredensialnya tersimpan terpisah namun tetap persisten antar-restart.

```bash
# Buat session baru (memulai pairing QR)
curl -X POST http://localhost:3000/sessions -d '{"name":"otp"}'

# Lihat semua session
curl http://localhost:3000/sessions

# Scan QR untuk session tsb
curl "http://localhost:3000/qr?session=otp&format=png" -o qr-otp.png

# Hapus session (logout + hapus kredensial)
curl -X DELETE http://localhost:3000/sessions/otp
```

Pada endpoint kirim pesan, sertakan field `"session"` (default `"default"`).

## Konfigurasi (Environment Variables)

| Variable | Default | Keterangan |
|---|---|---|
| `PORT` | `3000` | Port REST API |
| `API_KEY` | _(kosong)_ | Jika diisi, semua endpoint (kecuali `/health`) wajib header `X-API-Key` |
| `WEBHOOK_URL` | _(kosong)_ | URL tujuan pesan masuk (POST JSON). Kosong = nonaktif |
| `WEBHOOK_EVENTS` | `message` | Event yang diteruskan (`message`, atau `*` untuk semua) |
| `DATABASE_URL` | _(wajib)_ | DSN PostgreSQL, mis. `postgres://wa:wa_secret@postgres:5432/wa_gateway?sslmode=disable` |
| `STORE_DIR` | `./data` | Folder penyimpanan lokal (media backend `disk`, aset) |
| `DEFAULT_COUNTRY_CODE` | _(kosong)_ | Auto-konversi nomor lokal `0...` → internasional (mis. `62` ⇒ `0811...` jadi `62811...`) |
| `DOWNLOAD_MEDIA` | `true` | Unduh media masuk & sertakan base64 di webhook |
| `MAX_DOWNLOAD_BYTES` | `20971520` | Lewati unduh media yang lebih besar dari ini (20MB) |
| `STORE_MESSAGES` | `false` | Simpan pesan masuk & keluar ke tabel `gw_messages` (aktifkan untuk `GET /messages`) |
| `MESSAGE_RETENTION_DAYS` | `0` | Hapus otomatis pesan lebih tua dari N hari (`0` = selamanya). Untuk catch-up CRM, ≥ durasi offline terburuk |
| `STORE_MEDIA` | `false` | Simpan byte media ke storage (butuh `STORE_MESSAGES=true`); ambil via `GET /messages/{id}/media` |
| `MEDIA_BACKEND` | `disk` | Backend media: `disk` atau `s3` (MinIO/S3-compatible) |
| `MEDIA_DIR` | _(kosong)_ | Direktori media untuk backend `disk` (kosong = `<STORE_DIR>/media`) |
| `S3_ENDPOINT` | _(kosong)_ | Host:port MinIO/S3 (untuk `MEDIA_BACKEND=s3`), mis. `minio:9000` |
| `S3_BUCKET` | _(kosong)_ | Nama bucket media (dibuat otomatis bila belum ada) |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | _(kosong)_ | Kredensial MinIO/S3 |
| `S3_USE_SSL` | `false` | `true` bila endpoint MinIO/S3 pakai HTTPS |
| `S3_REGION` | _(kosong)_ | Region S3 (opsional; MinIO boleh dikosongkan) |
| `STORE_CHATS` | _(kosong)_ | Allowlist nomor/JID yang disimpan (comma). Grup pakai JID `...@g.us`. Kosong = semua |
| `STORE_CHATS_EXCLUDE` | _(kosong)_ | Blocklist nomor/JID (diabaikan bila `STORE_CHATS` diisi) |
| `BULK_MIN_DELAY_MS` | `3000` | Jeda minimum antar-pesan saat kirim massal (anti-ban) |
| `BULK_MAX_DELAY_MS` | `6000` | Jeda maksimum antar-pesan (jitter acak antara min–max) |
| `BULK_AUTO_RESUME` | `true` | Auto-resume job bulk yang terputus saat crash/restart (kirim ulang hanya penerima `pending`) |
| `DEFAULT_RATE_LIMIT` | `0` | Default batas request per window untuk key baru (`0` = tanpa batas) |
| `DEFAULT_RATE_WINDOW_SEC` | `60` | Default panjang window rate limit (detik) untuk key baru |
| `DEFAULT_MAX_SESSIONS` | `0` | Default batas jumlah session/device per key baru (`0` = tanpa batas) |
| `ACCESS_LOG_RETENTION_DAYS` | `7` | Simpan access log N hari; `0` = nonaktifkan pencatatan |
| `WEBHOOK_WORKERS` | `4` | Jumlah worker pengirim webhook paralel |
| `WEBHOOK_QUEUE_SIZE` | `1000` | Kapasitas antrian; pesan baru di-drop bila penuh |
| `WEBHOOK_MAX_RETRIES` | `3` | Jumlah retry setelah percobaan pertama gagal |
| `WEBHOOK_BACKOFF_MS` | `2000` | Backoff dasar (eksponensial: 2s, 4s, 8s, ...) |
| `LOG_LEVEL` | `INFO` | `DEBUG`/`INFO`/`WARN`/`ERROR` |

## REST API

Semua endpoint (kecuali `/health`) butuh header `X-API-Key: <API_KEY>` **jika** `API_KEY` di-set
**atau** ada managed key terdaftar. Header `Authorization: Bearer <key>` juga diterima.
Lihat [API Key Management](#api-key-management) untuk membuat banyak key dengan rate limit & scope.

> 📖 **Referensi lengkap:** [`openapi.yaml`](openapi.yaml) (OpenAPI 3.0, machine-readable) ·
> [`docs/copilot-api.md`](docs/copilot-api.md) (snippet siap-tempel + contoh TypeScript/Python/Go).

Format nomor `to`: nomor internasional tanpa `+` (mis. `628123456789`), atau JID grup (`xxxx@g.us`).
⚠️ **Jangan pakai awalan `0`** (format lokal) — gunakan kode negara (Indonesia = `62`).
Jika `DEFAULT_COUNTRY_CODE` di-set, nomor `0...` otomatis dikonversi (mis. `08114100444` → `6281122334455`).
Field `session` opsional di setiap request kirim (default `"default"`).

### `GET /health`
Health check. `{ "status": "ok" }`

### `GET /status` · `GET /status?session=otp`
Tanpa parameter: daftar semua session. Dengan `?session=`: status satu session.
```json
{ "sessions": [ { "name": "default", "connected": true, "loggedIn": true, "jid": "628...@s.whatsapp.net", "hasQR": false } ] }
```

### `GET /sessions` · `POST /sessions` · `DELETE /sessions/{name}`
Kelola multi-session.
```bash
curl http://localhost:3000/sessions
curl -X POST http://localhost:3000/sessions -d '{"name":"otp"}'
curl -X DELETE http://localhost:3000/sessions/otp
```

### `GET /qr` · `GET /qr?session=otp&format=png`
QR pairing. Default JSON `{ "code": "...", "pngBase64": "..." }`; `?format=png` mengembalikan gambar PNG.

### `POST /pair`
Pairing via **kode** (alternatif QR) — berguna saat WhatsApp menolak scan QR ("Can't link new devices right now"). Mengembalikan kode 8 karakter yang dimasukkan di **WhatsApp > Perangkat Tertaut > Tautkan dengan nomor telепон**.
```bash
curl -X POST http://localhost:3000/pair \
  -H "Content-Type: application/json" \
  -d '{"phone":"628123456789"}'
# { "code": "ABCD-EFGH", "phone": "628123456789", "hint": "..." }
```
Nomor harus format internasional tanpa `+`/`0` (atau set `DEFAULT_COUNTRY_CODE` agar `08...` dikonversi otomatis). Field `session` opsional (default `default`).

### `GET /groups` · `GET /groups?session=otp`
Daftar group yang diikuti akun. Gunakan `jid` hasilnya sebagai field `to` untuk mengirim ke group.
```bash
curl http://localhost:3000/groups
# {
#   "groups": [
#     { "jid": "120363xxxxxxxx@g.us", "name": "Kelas AI Tutor", "participants": 42, "isAnnounce": false }
#   ]
# }
```

> **Kirim ke group:** semua endpoint `/send/*` menerima JID group (`...@g.us`) di field `to`.
> Jika `isAnnounce: true`, hanya admin yang boleh mengirim ke group tersebut.

### `GET /messages`
Riwayat pesan masuk & keluar. **Hanya aktif bila `STORE_MESSAGES=true`** (kalau tidak: `501 Not Implemented`).

Query params (semua opsional): `session`, `chat` (JID lawan bicara / group), `limit` (default 100, maks 1000), `before` (unix-seconds untuk paginasi — ambil pesan lebih lama dari nilai ini).
```bash
curl "http://localhost:3000/messages?chat=120363xxxxxxxx@g.us&limit=50"
# {
#   "count": 2,
#   "messages": [
#     { "id": "3EB0...", "session": "default", "chat": "120363xxxxxxxx@g.us",
#       "sender": "628123456789@s.whatsapp.net", "direction": "in", "fromMe": false,
#       "isGroup": true, "type": "text", "body": "Halo tutor", "timestamp": 1717000000 }
#   ]
# }
```
Pesan diurutkan **terbaru dulu**. `direction` bernilai `in` (masuk) atau `out` (keluar). Untuk media hanya metadata `type` yang disimpan (isi file tidak), jadi tabel tetap ringan.

### `POST /send/text`
```bash
curl -X POST http://localhost:3000/send/text \
  -H 'Content-Type: application/json' \
  -d '{ "session": "default", "to": "628123456789", "text": "Halo! 👋" }'
```

Kirim ke group (pakai JID `@g.us` dari `GET /groups`):
```bash
curl -X POST http://localhost:3000/send/text \
  -H 'Content-Type: application/json' \
  -d '{ "to": "120363xxxxxxxx@g.us", "text": "Halo semua! 👋" }'
```

### `POST /send/image`
```bash
curl -X POST http://localhost:3000/send/image \
  -H 'Content-Type: application/json' \
  -d '{
        "to": "628123456789",
        "caption": "Ini gambarnya",
        "file": { "url": "https://example.com/foto.jpg" }
      }'
```

### `POST /send/file` (dokumen)
```bash
curl -X POST http://localhost:3000/send/file \
  -H 'Content-Type: application/json' \
  -d '{
        "to": "628123456789",
        "filename": "materi.pdf",
        "mimetype": "application/pdf",
        "file": { "url": "https://example.com/materi.pdf" }
      }'
```

### `POST /send/voice` (voice note / PTT)
Audio sebaiknya format **OGG/Opus** agar tampil sebagai voice note.
```bash
curl -X POST http://localhost:3000/send/voice \
  -H 'Content-Type: application/json' \
  -d '{
        "to": "628123456789",
        "seconds": 7,
        "mimetype": "audio/ogg; codecs=opus",
        "file": { "url": "https://example.com/jawaban.ogg" }
      }'
```

Media bisa dikirim via `file.url` (URL publik) **atau** `file.base64` (data base64).
Respon sukses: `{ "sent": true, "messageId": "..." }`.

### `POST /send/bulk` (kirim massal) · `GET /send/bulk` · `GET /send/bulk/{id}`
WhatsApp **tidak** punya API "kirim sekali ke banyak nomor" — pengiriman tetap satu per satu. Endpoint ini melakukan **loop di sisi server** secara **asinkron** dengan **jeda + jitter acak** (anti-ban), jadi cukup satu request.

Cara A — pesan sama ke banyak nomor:
```bash
curl -X POST http://localhost:3000/send/bulk \
  -H 'Content-Type: application/json' \
  -d '{
        "session": "default",
        "to": ["628123456789", "628987654321"],
        "text": "Pengumuman: kelas mulai jam 8 🚀",
        "minDelayMs": 3000,
        "maxDelayMs": 6000
      }'
```

Cara B — pesan berbeda per nomor (personalisasi penuh):
```bash
curl -X POST http://localhost:3000/send/bulk \
  -d '{ "messages": [
          { "to": "628123456789", "text": "Hai Budi, tugasmu sudah dinilai." },
          { "to": "628987654321", "text": "Hai Sari, jangan lupa kuis besok." }
      ] }'
```

Cara C — **template + variabel** (paling praktis untuk personalisasi nama/nilai/waktu):
```bash
curl -X POST http://localhost:3000/send/bulk \
  -d '{
        "template": "Halo {{name}}, nilaimu {{nilai}}. Kelas mulai {{waktu}} 📚",
        "messages": [
          { "to": "628123456789", "vars": { "name": "Budi", "nilai": "90", "waktu": "08:00" } },
          { "to": "628987654321", "vars": { "name": "Sari", "nilai": "85", "waktu": "09:00" } }
        ]
      }'
```
Placeholder berformat `{{nama_variabel}}` (boleh ada spasi: `{{ nama }}`) diganti dari `vars` tiap penerima.
- **Urutan prioritas teks:** `messages[].text` (override penuh) → render `template` dengan `vars` → `text` global.
- Placeholder yang tak ada di `vars` **dibiarkan apa adanya** (tidak dikosongkan), supaya kesalahan ketik mudah terlihat.
- Bisa dikombinasi: sebagian penerima pakai `vars`, sebagian pakai `text` sendiri.

Respon langsung (HTTP 202) berisi `id` job — pengiriman berjalan di background:
```json
{ "id": "fb49821e05e4e9de", "session": "default", "status": "running", "total": 2, "sent": 0, "failed": 0 }
```

Pantau progres & hasil per-nomor:
```bash
curl http://localhost:3000/send/bulk/fb49821e05e4e9de
# {
#   "status": "completed", "total": 2, "sent": 2, "failed": 0,
#   "results": [
#     { "to": "628123456789", "status": "sent", "messageId": "3EB0..." },
#     { "to": "628987654321", "status": "sent", "messageId": "3EB0..." }
#   ]
# }
```
`GET /send/bulk` mendaftar semua job (terbaru dulu). Job disimpan di SQLite secara permanen — tersedia di `GET /send/bulk/{id}` meski setelah restart.

> **Auto-resume (crash recovery):** Jika service restart/crash saat job berjalan,
> saat startup gateway otomatis melanjutkan job — **hanya** mengirim penerima yang
> belum pernah dicoba (`status:"pending"`). Penerima yang sudah `sent`/`failed`
> dilewati sehingga **tidak ada duplikat** (kecuali pada celah langka di mana pesan
> sempat terkirim ke WhatsApp namun status-nya gagal tersimpan sebelum crash).
> Resume menunggu session siap (login) maks. 30 detik; jika belum siap, job tetap
> berstatus `interrupted` untuk dicoba lagi nanti. Nonaktifkan via `BULK_AUTO_RESUME=false`
> (job hanya ditandai `interrupted`, penanganan manual).

> ⚠️ **Hindari ban:** jangan kirim ke ribuan nomor sekaligus / tanpa jeda. Default jeda 3–6 detik per pesan diatur via `BULK_MIN_DELAY_MS`/`BULK_MAX_DELAY_MS` dan bisa ditimpa per-request. Kirim hanya ke nomor yang menyetujui (opt-in).

### `POST /logout`
Logout device pada session (perlu scan QR lagi setelahnya). Body: `{ "session": "default" }`.

## API Key Management

Selain `API_KEY` tunggal (env), gateway mendukung **banyak managed key** dengan
rate limit, batas session/device, scope, expiry, dan enable/disable per key.

- `API_KEY` (env) berfungsi sebagai **master key** — akses penuh, tanpa limit,
  dipakai untuk mengelola managed key. Endpoint `/admin/*` butuh scope `admin`
  (master key otomatis memenuhi).
- Plaintext secret **hanya ditampilkan sekali** saat create/rotate. DB hanya
  menyimpan hash SHA-256. Format key: `wag_` + 40 hex.
- **Scopes**: `send` (kirim/normalize/check), `read` (status/qr/groups/messages),
  `sessions` (create/delete/pair/logout), `admin` (kelola key), `*` (semua).
- **Rate limit**: fixed-window. Saat terlampaui → `429` + header
  `X-RateLimit-Limit`/`X-RateLimit-Remaining`/`X-RateLimit-Reset` + `Retry-After`.
- **Batas device**: `maxSessions` diperiksa saat `POST /sessions`; session ditandai
  `owner_key`.

Endpoint (semua butuh scope `admin`):

| Method | Path | Fungsi |
|---|---|---|
| `POST` | `/admin/keys` | Buat key (mengembalikan `secret` sekali) |
| `GET` | `/admin/keys` | List key (tanpa secret) |
| `GET` | `/admin/keys/{id}` | Detail key |
| `PATCH` | `/admin/keys/{id}` | Ubah (name, scopes, rateLimit, rateWindowSec, maxSessions, enabled, expiresAt) |
| `POST` | `/admin/keys/{id}/enable` | Aktifkan key (shortcut `enabled:true`) |
| `POST` | `/admin/keys/{id}/disable` | Nonaktifkan key (shortcut `enabled:false`) |
| `POST` | `/admin/keys/{id}/rotate` | Ganti secret (mengembalikan secret baru) |
| `DELETE` | `/admin/keys/{id}` | Hapus key |

Contoh buat key (rate limit 100/menit, maksimal 2 session, hanya kirim & baca):
```bash
curl -X POST http://localhost:3000/admin/keys \
  -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
  -d '{
    "name": "app-otp",
    "scopes": ["send", "read"],
    "rateLimit": 100,
    "rateWindowSec": 60,
    "maxSessions": 2
  }'
```
Respons (simpan `secret`, tidak bisa dilihat lagi):
```json
{
  "id": "key_3f1c...",
  "name": "app-otp",
  "prefix": "wag_8a2b1c0d",
  "scopes": ["send", "read"],
  "rateLimit": 100,
  "rateWindowSec": 60,
  "maxSessions": 2,
  "enabled": true,
  "createdAt": 1717000000,
  "secret": "wag_8a2b1c0d..."
}
```
Gunakan key: `-H "X-API-Key: wag_8a2b1c0d..."` atau `-H "Authorization: Bearer wag_..."`.

Nonaktifkan / aktifkan ulang (endpoint khusus, atau via `PATCH`):
```bash
# Cara cepat — endpoint dedicated
curl -X POST http://localhost:3000/admin/keys/key_3f1c.../disable -H "X-API-Key: $API_KEY"
curl -X POST http://localhost:3000/admin/keys/key_3f1c.../enable  -H "X-API-Key: $API_KEY"

# Atau via PATCH
curl -X PATCH http://localhost:3000/admin/keys/key_3f1c... \
  -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
  -d '{"enabled": false}'
```

## Access Log Monitoring

Setiap request yang terautentikasi dicatat ke SQLite secara async (buffer flush
tiap 5 detik). Log otomatis dihapus sesuai `ACCESS_LOG_RETENTION_DAYS`.

| Method | Path | Fungsi |
|---|---|---|
| `GET` | `/admin/logs` | Semua log (filter: `?key=`, `?since=`, `?limit=`) |
| `GET` | `/admin/keys/{id}/logs` | Log untuk satu key spesifik |

```bash
# 100 log terbaru
curl -H "X-API-Key: $API_KEY" http://localhost:3000/admin/logs

# Log key tertentu sejak 1 jam lalu
SINCE=$(date -d '1 hour ago' +%s 2>/dev/null || date -v-1H +%s)
curl -H "X-API-Key: $API_KEY" \
  "http://localhost:3000/admin/keys/key_3f1c.../logs?since=$SINCE&limit=500"
```

Lihat [docs/access-log.md](docs/access-log.md) untuk dokumentasi lengkap,
schema database, dan contoh integrasi TypeScript/Python.

## Webhook (Pesan Masuk)

Jika `WEBHOOK_URL` di-set, setiap pesan masuk dikirim sebagai POST JSON:

```json
{
  "event": "message",
  "session": "default",
  "payload": {
    "id": "ABCD1234",
    "timestamp": 1717000000,
    "from": "628123456789@s.whatsapp.net",
    "sender": "628123456789@s.whatsapp.net",
    "pushName": "Budi",
    "fromMe": false,
    "isGroup": false,
    "type": "image",
    "body": "tolong jelaskan soal ini",
    "hasMedia": true,
    "media": {
      "mimetype": "image/jpeg",
      "fileLength": 53120,
      "dataBase64": "/9j/4AAQSkZJRg..."
    }
  }
}
```

`type` bisa: `text`, `image`, `video`, `audio`, `document`, `sticker`, `unknown`.
Untuk pesan teks, isi ada di `body`. Untuk media, `media.dataBase64` berisi file (jika `DOWNLOAD_MEDIA=true` dan ukuran ≤ `MAX_DOWNLOAD_BYTES`).
Field `session` menunjukkan nomor/sesi mana yang menerima pesan.

Pengiriman webhook melewati **antrian dengan worker pool**. Bila endpoint-mu gagal
(non-2xx atau timeout), gateway otomatis **retry dengan backoff eksponensial**
sebanyak `WEBHOOK_MAX_RETRIES` kali sebelum menyerah.

## Struktur Proyek

```
.
├── main.go                       # entrypoint: wiring + graceful shutdown
├── internal/
│   ├── config/config.go          # load konfigurasi dari env
│   ├── gateway/
│   │   ├── manager.go            # kelola banyak session + store (multi-session)
│   │   ├── session.go            # satu koneksi WA: connect, QR, pair, kirim (text/media/voice)
│   │   ├── webhook.go            # antrian webhook + retry/backoff
│   │   ├── store.go              # persistensi riwayat pesan (gw_messages)
│   │   ├── bulk.go               # bulk sender async + auto-resume (gw_bulk_*)
│   │   ├── apikey.go             # API key management (scope, rate limit, rotate)
│   │   └── accesslog.go          # access log monitoring
│   └── api/server.go             # REST API + auth middleware
├── cmd/wagctl/                   # CLI: kelola key, pairing, kirim pesan
├── docs/                         # copilot-api.md, access-log.md
├── openapi.yaml                  # spesifikasi OpenAPI 3.0
├── Dockerfile
├── docker-compose.yml
└── .env.example
```

## Healthcheck Docker

Sudah disertakan di `docker-compose.yml` (memanggil `/health` dengan `wget` bawaan alpine):

```yaml
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

Cek status container: `docker compose ps` (kolom STATUS akan menampilkan `healthy`).

## Catatan Keamanan

- Folder `data/` berisi kredensial WhatsApp — **jangan di-commit** (sudah di `.gitignore`).
- Selalu set `API_KEY` di lingkungan produksi.
- Letakkan di belakang reverse proxy (HTTPS) bila diekspos ke internet.
