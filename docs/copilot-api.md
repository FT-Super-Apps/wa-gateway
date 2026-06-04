# WA Gateway — API Reference for Copilot

> Tambahkan blok ini ke `.github/copilot-instructions.md` project Anda agar Copilot
> memahami cara memanggil WA Gateway.

---

## Service Info

- **Base URL:** `http://localhost:3111` (dev) atau sesuai `WA_GATEWAY_URL` env
- **Auth:** header `X-API-Key: <key>` (atau `Authorization: Bearer <key>`) pada semua endpoint kecuali `/health`
- **API key:** bisa pakai `API_KEY` env (master) **atau** managed key dari `/admin/keys` (lihat [API Key Management](#api-key-management))
- **Format:** JSON request & response, `Content-Type: application/json`
- **OpenAPI spec:** `openapi.yaml` di root repo `FT-Super-Apps/wa-gateway`

## Phone Number Format

| Input | Hasil |
|-------|-------|
| `628114100444` | ✅ Internasional tanpa `+` |
| `+628114100444` | ✅ Diterima (tanda `+` di-strip) |
| `08114100444` | ✅ Jika `DEFAULT_COUNTRY_CODE=62` di-set |
| `0812-345-678` | ✅ Separator apa saja di-strip otomatis |
| `+6281-234-5678` | ✅ Format internasional dengan separator |
| `(628) 114.100 444` | ✅ Kurung, titik, spasi di-strip |
| `1234567890123456789@g.us` | ✅ Group JID langsung diteruskan |
| `08114100444` tanpa `DEFAULT_COUNTRY_CODE` | ❌ Ditolak |

---

## Endpoints

### Health
```
GET /health
→ {"status":"ok"}
```
Tidak memerlukan auth. Gunakan untuk liveness check.

---

### Normalisasi Nomor Telepon
```http
POST /normalize
{
  "phones": ["0812-345-678", "+6281-234-5678", "(628) 114.100 444"],
  "countryCode": "62"   // opsional, default dari DEFAULT_COUNTRY_CODE
}
→ {
    "results": [
      {"input": "0812-345-678",      "normalized": "62812345678"},
      {"input": "+6281-234-5678",    "normalized": "62812345678"},
      {"input": "(628) 114.100 444", "normalized": "628114100444"},
      {"input": "abc",               "error": "phone number contains no digits"}
    ]
  }
```

> **Tips:** Panggil `/normalize` dulu untuk sanitasi input pengguna sebelum memanggil
> `/check` atau endpoint pengiriman. Nomor yang `error` tidak perlu dikirim.

---

### Cek Nomor WhatsApp
```http
POST /check
{
  "session": "default",                         // opsional
  "phones": ["628114100444", "628222333444"]    // maks 250 nomor
}
→ {
    "count": 2,
    "results": [
      {
        "phone": "628114100444",
        "jid": "628114100444@s.whatsapp.net",
        "isOnWhatsApp": true,
        "isBusiness": false
      },
      {
        "phone": "628222333444",
        "jid": "628222333444@s.whatsapp.net",
        "isOnWhatsApp": false,
        "isBusiness": false
      }
    ]
  }
```

> **Tips:** Gunakan `isOnWhatsApp: true` sebagai gate sebelum kirim OTP/notifikasi
> agar tidak membuang kuota ke nomor yang tidak aktif di WA.

---

### Kirim Teks
```http
POST /send/text
{
  "session": "default",     // opsional, default "default"
  "to": "628114100444",     // nomor atau group JID (@g.us)
  "text": "Kode OTP Anda: 123456"
}
→ {"sent":true,"messageId":"3EB0..."}
```

### Kirim Gambar
```http
POST /send/image
{
  "session": "default",
  "to": "628114100444",
  "caption": "Bukti pembayaran",
  "file": {
    "url": "https://example.com/invoice.jpg"
    // atau: "base64": "<base64-encoded-content>"
  }
}
→ {"sent":true,"messageId":"..."}
```

### Kirim File/Dokumen
```http
POST /send/file
{
  "to": "628114100444",
  "filename": "laporan.pdf",
  "mimetype": "application/pdf",
  "file": { "url": "https://example.com/laporan.pdf" }
}
→ {"sent":true,"messageId":"..."}
```

### Kirim Voice Note
```http
POST /send/voice
{
  "to": "628114100444",
  "seconds": 10,                           // durasi (opsional, info saja)
  "mimetype": "audio/ogg; codecs=opus",
  "file": { "base64": "<ogg-opus-base64>" }
}
→ {"sent":true,"messageId":"..."}
```

---

### Bulk Send (Async)

```http
POST /send/bulk
→ 202 Accepted: {"id":"a1b2c3d4","status":"running","total":3,...}
```

**Broadcast teks sama ke banyak nomor:**
```json
{
  "to": ["628111","628222","628333"],
  "text": "Promo diskon 50% hari ini!"
}
```

**Pesan personal dengan template:**
```json
{
  "template": "Halo {{name}}, nilai Anda {{nilai}}.",
  "messages": [
    {"to": "628111", "vars": {"name": "Budi", "nilai": "90"}},
    {"to": "628222", "vars": {"name": "Ani",  "nilai": "85"}}
  ]
}
```

**Semua field BulkRequest:**
| Field | Tipe | Keterangan |
|-------|------|------------|
| `session` | string | Nama session (default: `"default"`) |
| `to` | string[] | Penerima yang mendapat teks/template yang sama |
| `text` | string | Teks broadcast ke semua `to` |
| `template` | string | Template dengan `{{placeholder}}` |
| `messages` | BulkMessage[] | Pesan per-penerima (mengesampingkan `to`) |
| `minDelayMs` | int | Delay minimum antar kirim (default dari env) |
| `maxDelayMs` | int | Delay maksimum antar kirim (default dari env) |

**Prioritas teks per penerima:** `messages[i].text` > `render(template, vars)` > `text` global

### List Semua Bulk Job
```http
GET /send/bulk
→ {"jobs":[{"id":"...","status":"completed","total":10,"sent":10,...},...]}
```

### Cek Status Bulk Job
```http
GET /send/bulk/{id}
→ {
    "id": "a1b2c3d4",
    "status": "completed",   // "running" | "completed" | "cancelled" | "interrupted"
    "total": 3, "sent": 3, "failed": 0,
    "startedAt": 1700000000,
    "finishedAt": 1700000005,
    "results": [
      {"to":"628111","status":"sent","messageId":"..."},
      {"to":"628222","status":"failed","error":"..."},
      {"to":"628333","status":"pending"}
    ]
  }
```

> **Status `interrupted` & auto-resume:** Jika service restart/crash saat job
> berjalan, gateway **otomatis melanjutkan** saat startup — hanya mengirim penerima
> dengan `status:"pending"` (belum pernah dicoba). Penerima yang sudah `sent`/`failed`
> dilewati → tidak ada duplikat. Jika session belum siap (login) dalam 30 detik, job
> tetap `interrupted` untuk dicoba lagi nanti. Auto-resume bisa dinonaktifkan dengan
> `BULK_AUTO_RESUME=false` (job hanya ditandai `interrupted`, tanpa kirim ulang).

---

### Session Management
```http
GET  /sessions                       → {"sessions":[...]}
POST /sessions  {"name":"otp"}       → 201 SessionStatus
DELETE /sessions/{name}              → {"removed":true}

GET  /status                         → semua session (array SessionStatus)
GET  /status?session=default         → satu SessionStatus
```

### Pairing WhatsApp

**Opsi 1 – QR Code:**
```http
GET /qr?session=default&format=png   → gambar PNG
GET /qr?session=default              → {"code":"...", "pngBase64":"..."}
```

**Opsi 2 – Pairing Code (direkomendasikan untuk server headless):**
```http
POST /pair
{"session":"default","phone":"628114100444"}
→ {"code":"ABCD-1234","phone":"628114100444","hint":"WhatsApp > Linked Devices > Link with phone number instead"}
```

### Riwayat Pesan
> Memerlukan `STORE_MESSAGES=true`
```http
GET /messages?session=default&chat=628111@s.whatsapp.net&limit=50&before=1700000000
→ {"messages":[...],"count":50}
```

### Daftar Group
```http
GET /groups?session=default
→ {"groups":[{"jid":"...@g.us","name":"Tim Produk","participants":12}]}
```

### Logout
```http
POST /logout {"session":"default"}
→ {"loggedOut":true}
```

### API Key Management

> Butuh scope `admin`. Master key (`API_KEY` env) otomatis memenuhi. Secret
> hanya muncul sekali saat create/rotate — simpan segera.

```http
POST /admin/keys
{
  "name": "app-otp",
  "scopes": ["send", "read"],   // "*"|"send"|"read"|"sessions"|"admin"
  "rateLimit": 100,              // request per window; 0 = unlimited
  "rateWindowSec": 60,
  "maxSessions": 2,             // batas device; 0 = unlimited
  "expiresAt": 0                // unix detik; 0 = tak kedaluwarsa
}
→ 201 { "id":"key_...", "prefix":"wag_8a2b1c0d", "secret":"wag_...", ... }

GET    /admin/keys              → { "keys": [ {APIKey}, ... ] }   // tanpa secret
GET    /admin/keys/{id}         → {APIKey}
PATCH  /admin/keys/{id}         {"enabled":false}   → {APIKey}
POST   /admin/keys/{id}/rotate  → {APIKey + secret baru}
DELETE /admin/keys/{id}         → {"deleted":true}

// Access log monitoring
GET    /admin/logs              → { "logs": [{AccessLog},...], "count": N }
GET    /admin/keys/{id}/logs    → { "logs": [{AccessLog},...], "count": N }
// Query params: key (key_id), since (unix timestamp), limit (default 100, maks 1000)
```

Saat rate limit terlampaui → `429` dengan header `X-RateLimit-Limit`,
`X-RateLimit-Remaining`, `X-RateLimit-Reset` (unix), dan `Retry-After` (detik).

---

## Access Log Monitoring

Setiap request yang berhasil diautentikasi dicatat otomatis — termasuk request yang
kena rate limit. Log disimpan di SQLite dan dibersihkan otomatis sesuai
`ACCESS_LOG_RETENTION_DAYS`.

### Endpoints

```
GET /admin/logs
GET /admin/logs?key=<keyId>&since=<unix>&limit=<n>
GET /admin/keys/{id}/logs
GET /admin/keys/{id}/logs?since=<unix>&limit=<n>
```

Butuh scope **`admin`** (atau master key).

### Query Parameters

| Parameter | Tipe | Default | Keterangan |
|-----------|------|---------|------------|
| `key` | string | — | Filter berdasarkan key ID (hanya di `/admin/logs`) |
| `since` | integer | — | Unix timestamp; hanya tampilkan log setelah waktu ini |
| `limit` | integer | 100 | Jumlah maksimal entri (maks 1000) |

### Response

```json
{
  "count": 2,
  "logs": [
    {
      "id": 42,
      "keyId": "key_a1b2c3d4",
      "keyName": "app-otp",
      "method": "POST",
      "path": "/send/text",
      "statusCode": 200,
      "latencyMs": 312,
      "ip": "10.0.0.5",
      "createdAt": 1717459200
    },
    {
      "id": 41,
      "keyId": "key_a1b2c3d4",
      "keyName": "app-otp",
      "method": "POST",
      "path": "/send/text",
      "statusCode": 429,
      "latencyMs": 1,
      "ip": "10.0.0.5",
      "createdAt": 1717459195
    }
  ]
}
```

Hasil diurutkan **terbaru dulu** (`createdAt DESC`).

### Contoh curl

```bash
# Semua log 100 entri terakhir
curl -H "X-API-Key: $MASTER_KEY" http://localhost:3111/admin/logs

# Log untuk key tertentu, 200 entri terakhir
curl -H "X-API-Key: $MASTER_KEY" \
  "http://localhost:3111/admin/logs?key=key_a1b2c3d4&limit=200"

# Log sejak 1 jam lalu
SINCE=$(date -d '1 hour ago' +%s 2>/dev/null || date -v-1H +%s)
curl -H "X-API-Key: $MASTER_KEY" \
  "http://localhost:3111/admin/logs?since=$SINCE"

# Log via endpoint per-key
curl -H "X-API-Key: $MASTER_KEY" \
  "http://localhost:3111/admin/keys/key_a1b2c3d4/logs?limit=50"
```

### Konfigurasi

| Env Var | Default | Keterangan |
|---------|---------|------------|
| `ACCESS_LOG_RETENTION_DAYS` | `7` | Hapus log lebih tua dari N hari. `0` = fitur nonaktif |

> **Catatan:** Jika `ACCESS_LOG_RETENTION_DAYS=0`, endpoint `/admin/logs` tetap ada
> tetapi selalu mengembalikan `{"logs": null, "count": 0}`.

### Cara kerja internal

- Log di-buffer di memory, di-flush ke SQLite tiap **5 detik**.
- Auto-purge jalan saat startup dan setiap **24 jam**.
- Setiap request yang berhasil diautentikasi dicatat, termasuk:
  - Request normal (2xx, 4xx, 5xx)
  - Request yang kena rate limit (429)
  - Request tanpa auth (`ACCESS_LOG_RETENTION_DAYS > 0` + no `API_KEY` set → key `"master"`)
- Request yang gagal autentikasi (401 invalid key) **tidak** dicatat.

---

## Common Response Shapes

```typescript
// Success send
{ sent: true, messageId: string }

// SessionStatus
{
  name: string
  connected: boolean
  loggedIn: boolean
  jid?: string        // "6281234@s.whatsapp.net"
  pushName?: string
  hasQR: boolean
  pairError?: string
}

// Error (semua endpoint error)
{ error: string }
```

## HTTP Status Codes

| Code | Arti |
|------|------|
| 200 | OK |
| 201 | Session dibuat |
| 202 | Bulk job diterima |
| 400 | Request tidak valid |
| 401 | API key salah/kosong |
| 403 | Scope tidak cukup / key disabled / expired / batas session tercapai |
| 404 | Session/resource tidak ditemukan |
| 409 | Konflik (session sudah ada / sudah login) |
| 429 | Rate limit terlampaui (cek header `Retry-After`) |
| 501 | Fitur dinonaktifkan (message storage) |
| 502 | Gagal kirim ke WhatsApp |

---

## Contoh Integrasi (TypeScript)

```typescript
const WA_URL = process.env.WA_GATEWAY_URL ?? 'http://localhost:3111';
const WA_KEY = process.env.WA_GATEWAY_API_KEY ?? '';

const headers = { 'Content-Type': 'application/json', 'X-API-Key': WA_KEY };

async function sendOTP(phone: string, otp: string): Promise<string> {
  // Normalisasi dulu sebelum kirim
  const normRes = await fetch(`${WA_URL}/normalize`, {
    method: 'POST', headers,
    body: JSON.stringify({ phones: [phone] }),
  });
  const { results } = await normRes.json();
  if (results[0].error) throw new Error(`invalid phone: ${results[0].error}`);

  const res = await fetch(`${WA_URL}/send/text`, {
    method: 'POST', headers,
    body: JSON.stringify({ to: results[0].normalized, text: `Kode OTP Anda: ${otp}` }),
  });
  if (!res.ok) throw new Error((await res.json()).error);
  return (await res.json()).messageId;
}

async function sendBulkNotification(recipients: Array<{ phone: string; name: string }>) {
  const res = await fetch(`${WA_URL}/send/bulk`, {
    method: 'POST', headers,
    body: JSON.stringify({
      template: 'Halo {{name}}, pesanan Anda telah dikirim!',
      messages: recipients.map(r => ({ to: r.phone, vars: { name: r.name } })),
    }),
  });
  const job = await res.json();
  return job.id; // poll GET /send/bulk/{id} untuk status
}
```

## Contoh Integrasi (Python)

```python
import os, requests

WA_URL = os.getenv("WA_GATEWAY_URL", "http://localhost:3111")
HEADERS = {
    "Content-Type": "application/json",
    "X-API-Key": os.getenv("WA_GATEWAY_API_KEY", ""),
}

def normalize_phone(phone: str) -> str:
    r = requests.post(f"{WA_URL}/normalize", headers=HEADERS,
                      json={"phones": [phone]})
    r.raise_for_status()
    result = r.json()["results"][0]
    if "error" in result:
        raise ValueError(f"invalid phone {phone}: {result['error']}")
    return result["normalized"]

def send_otp(phone: str, otp: str) -> str:
    normalized = normalize_phone(phone)
    r = requests.post(f"{WA_URL}/send/text", headers=HEADERS, json={
        "to": normalized,
        "text": f"Kode OTP Anda: {otp}. Jangan bagikan ke siapapun.",
    })
    r.raise_for_status()
    return r.json()["messageId"]

def send_bulk(template: str, recipients: list[dict]) -> str:
    """recipients: [{"to": "628...", "vars": {"name": "Budi"}}]"""
    r = requests.post(f"{WA_URL}/send/bulk", headers=HEADERS, json={
        "template": template,
        "messages": recipients,
    })
    r.raise_for_status()
    return r.json()["id"]  # job ID untuk polling
```

## Contoh Integrasi (Go)

```go
package wagw

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
)

type Client struct {
    base   string
    apiKey string
    http   *http.Client
}

func NewClient(base, apiKey string) *Client {
    return &Client{base: base, apiKey: apiKey, http: &http.Client{}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
    b, _ := json.Marshal(body)
    req, _ := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(b))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-API-Key", c.apiKey)
    resp, err := c.http.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) SendText(ctx context.Context, to, text string) (string, error) {
    var result struct {
        MessageID string `json:"messageId"`
        Error     string `json:"error"`
    }
    if err := c.do(ctx, "POST", "/send/text", map[string]string{"to": to, "text": text}, &result); err != nil {
        return "", err
    }
    if result.Error != "" {
        return "", fmt.Errorf("wa-gateway: %s", result.Error)
    }
    return result.MessageID, nil
}
```

---

## CLI (`wagctl`)

`wagctl` adalah tool CLI bawaan untuk mengelola API key dan mengirim pesan dari terminal.

### Build & konfigurasi
```bash
go build -o wagctl ./cmd/wagctl/
export WA_GATEWAY_URL=http://localhost:3111
export WA_GATEWAY_API_KEY=<master-key>
```

### Referensi perintah

| Perintah | Fungsi |
|---|---|
| `wagctl keys list` | Daftar semua API key |
| `wagctl keys create --name=<n> [opts]` | Buat key baru (secret muncul sekali) |
| `wagctl keys get <id>` | Detail satu key |
| `wagctl keys update <id> [opts]` | Update atribut key |
| `wagctl keys rotate <id>` | Rotate secret |
| `wagctl keys delete <id> [--force]` | Hapus key |
| `wagctl status [--session=<n>]` | Status koneksi session |
| `wagctl check --phones=<p1,p2>` | Cek nomor di WhatsApp |
| `wagctl normalize --phones=<p1,p2>` | Normalisasi nomor |
| `wagctl send text --to=<p> --text=<t>` | Kirim pesan teks |
| `wagctl send image --to=<p> --url=<u>` | Kirim gambar |
| `wagctl send file --to=<p> --url=<u>` | Kirim file |

### Contoh skenario
```bash
# Setup key baru untuk app OTP
wagctl keys create --name="app-otp" --scopes="send,read" \
  --rate-limit=100 --rate-window=60 --max-sessions=2

# Nonaktifkan key yang bocor
wagctl keys update key_abc123 --enabled=false

# Test kirim pesan
wagctl send text --to="628114100444" --text="Test dari CLI"
```

---

## Environment Variables

### Di project consumer (yang memanggil API ini)
```env
WA_GATEWAY_URL=http://localhost:3111
WA_GATEWAY_API_KEY=your-secret-key
```

### Di service wa-gateway itu sendiri
```env
# Server
PORT=3000                          # default 3000
API_KEY=your-secret-key            # master key; kosongkan = tanpa auth (jika belum ada managed key)

# API key management (default untuk key baru via /admin/keys)
DEFAULT_RATE_LIMIT=0               # request per window; 0 = unlimited
DEFAULT_RATE_WINDOW_SEC=60         # panjang window rate limit (detik)
DEFAULT_MAX_SESSIONS=0             # batas session/device per key; 0 = unlimited

# Access log monitoring
ACCESS_LOG_RETENTION_DAYS=7        # simpan access log N hari; 0 = nonaktif

# Phone normalization
DEFAULT_COUNTRY_CODE=62            # untuk konversi nomor lokal 0xxx

# Storage
STORE_DIR=./data                   # direktori SQLite session store
STORE_MESSAGES=false               # simpan riwayat pesan ke DB
MESSAGE_RETENTION_DAYS=0           # 0 = selamanya

# Webhook (kosongkan WEBHOOK_URL = nonaktif)
WEBHOOK_URL=https://your-app/webhook
WEBHOOK_EVENTS=message             # "message" atau "*" untuk semua
WEBHOOK_WORKERS=4
WEBHOOK_QUEUE_SIZE=1000
WEBHOOK_MAX_RETRIES=3
WEBHOOK_BACKOFF_MS=2000
DOWNLOAD_MEDIA=true                # lampirkan media base64 ke webhook
MAX_DOWNLOAD_BYTES=209715200       # 200MB

# Bulk send delay antar pesan
BULK_MIN_DELAY_MS=3000
BULK_MAX_DELAY_MS=6000
# Auto-resume job bulk yang terputus (crash/restart); kirim ulang hanya penerima pending
BULK_AUTO_RESUME=true
```
