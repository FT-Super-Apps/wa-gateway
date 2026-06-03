# WA Gateway — API Reference for Copilot

> Tambahkan blok ini ke `.github/copilot-instructions.md` project Anda agar Copilot
> memahami cara memanggil WA Gateway.

---

## Service Info

- **Base URL:** `http://localhost:3111` (dev) atau sesuai `WA_GATEWAY_URL` env
- **Auth:** header `X-API-Key: <value>` pada semua endpoint kecuali `/health`
- **Format:** JSON request & response, `Content-Type: application/json`
- **OpenAPI spec:** `openapi.yaml` di root repo `FT-Super-Apps/wa-gateway`

## Phone Number Format

| Input | Hasil |
|-------|-------|
| `628114100444` | ✅ Internasional tanpa `+` |
| `+628114100444` | ✅ Diterima (tanda `+` di-strip) |
| `08114100444` | ✅ Jika `DEFAULT_COUNTRY_CODE=62` di-set |
| `1234567890123456789@g.us` | ✅ Group JID |
| `08114100444` tanpa country code | ❌ Ditolak |

---

## Endpoints

### Health
```
GET /health
→ {"status":"ok"}
```

### Cek Nomor WhatsApp
```http
POST /check
{
  "session": "default",         // opsional
  "phones": ["628114100444", "628222333444"]   // maks 250 nomor
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

### Kirim Teks
```http
POST /send/text
{
  "session": "default",   // opsional, default "default"
  "to": "628114100444",   // nomor atau group JID
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
```

### Kirim Voice Note
```http
POST /send/voice
{
  "to": "628114100444",
  "seconds": 10,
  "mimetype": "audio/ogg; codecs=opus",
  "file": { "base64": "<ogg-opus-base64>" }
}
```

### Bulk Send (Async)
```http
POST /send/bulk
→ 202 Accepted: {"id":"a1b2c3d4","status":"running","total":3,...}

// Broadcast sama ke banyak nomor:
{
  "to": ["628111","628222","628333"],
  "text": "Promo diskon 50% hari ini!"
}

// Pesan personal dengan template:
{
  "template": "Halo {{name}}, nilai Anda {{nilai}}.",
  "messages": [
    {"to": "628111", "vars": {"name": "Budi", "nilai": "90"}},
    {"to": "628222", "vars": {"name": "Ani",  "nilai": "85"}}
  ]
}
```

**Prioritas teks per penerima:** `messages[i].text` > `render(template, vars)` > `text` global

### Cek Status Bulk Job
```http
GET /send/bulk/{id}
→ {
    "id": "a1b2c3d4",
    "status": "completed",   // running | completed | cancelled
    "total": 3, "sent": 3, "failed": 0,
    "results": [
      {"to":"628111","status":"sent","messageId":"..."},
      ...
    ]
  }
```

### Session Management
```http
GET  /sessions                     → {"sessions":[...]}
POST /sessions  {"name":"otp"}     → 201 SessionStatus
DELETE /sessions/{name}            → {"removed":true}

GET  /status                       → semua session
GET  /status?session=default       → satu session
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
{"session":"default","phone":"6288108299111"}
→ {"code":"ABCD-1234","hint":"WhatsApp > Linked Devices > Link with phone number instead"}
```

### Riwayat Pesan (memerlukan `STORE_MESSAGES=true`)
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

---

## Common Response Shapes

```typescript
// Success send
{ sent: true, messageId: string }

// Session Status
{
  name: string
  connected: boolean
  loggedIn: boolean
  jid?: string       // "6281234@s.whatsapp.net"
  pushName?: string
  hasQR: boolean
  pairError?: string
}

// Error (semua error)
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
| 404 | Session/resource tidak ditemukan |
| 409 | Konflik (session sudah ada / sudah login) |
| 501 | Fitur dinonaktifkan (message storage) |
| 502 | Gagal kirim ke WhatsApp |

---

## Contoh Integrasi (TypeScript)

```typescript
const WA_URL = process.env.WA_GATEWAY_URL ?? 'http://localhost:3111';
const WA_KEY = process.env.WA_GATEWAY_API_KEY ?? '';

async function sendOTP(phone: string, otp: string): Promise<string> {
  const res = await fetch(`${WA_URL}/send/text`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-API-Key': WA_KEY },
    body: JSON.stringify({ to: phone, text: `Kode OTP Anda: ${otp}` }),
  });
  if (!res.ok) {
    const err = await res.json();
    throw new Error(err.error);
  }
  const data = await res.json();
  return data.messageId;
}

async function sendBulkNotification(recipients: Array<{ phone: string; name: string }>) {
  const res = await fetch(`${WA_URL}/send/bulk`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-API-Key': WA_KEY },
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

def send_otp(phone: str, otp: str) -> str:
    r = requests.post(f"{WA_URL}/send/text", headers=HEADERS, json={
        "to": phone,
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

func (c *Client) SendText(ctx context.Context, to, text string) (string, error) {
    body, _ := json.Marshal(map[string]string{"to": to, "text": text})
    req, _ := http.NewRequestWithContext(ctx, "POST", c.base+"/send/text", bytes.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-API-Key", c.apiKey)
    resp, err := c.http.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()
    var result struct {
        MessageID string `json:"messageId"`
        Error     string `json:"error"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    if result.Error != "" {
        return "", fmt.Errorf("wa-gateway: %s", result.Error)
    }
    return result.MessageID, nil
}
```

---

## Environment Variables (untuk referensi konfigurasi)

```env
WA_GATEWAY_URL=http://localhost:3111   # di project consumer
WA_GATEWAY_API_KEY=your-secret-key     # di project consumer

# Di service wa-gateway itu sendiri:
PORT=3111
API_KEY=your-secret-key
STORE_MESSAGES=true
DEFAULT_COUNTRY_CODE=62
```
