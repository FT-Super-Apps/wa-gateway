# Panduan Integrasi WA Gateway untuk Developer Consumer

Dokumen ini untuk developer aplikasi yang ingin **memakai** WA Gateway (bukan
mengembangkannya). Isinya alur kerja end-to-end: mendapatkan API key, mengirim
pesan pertama, menerima webhook, sampai memantau status pengiriman — plus resep
untuk kasus umum (OTP, notifikasi massal, chatbot/AI tutor).

> Referensi field per endpoint ada di [`openapi.yaml`](../openapi.yaml) dan
> [`copilot-api.md`](copilot-api.md) (snippet siap tempel ke `.github/copilot-instructions.md`
> project Anda). Dokumen ini fokus ke **cara pakai**, bukan daftar field.

---

## 1. Yang Anda butuhkan

| Item | Dari mana | Contoh |
|---|---|---|
| Base URL gateway | Admin gateway | `https://wa.example.ac.id` atau `http://localhost:3111` |
| API key | Admin membuatkan via `wagctl keys create` / `POST /admin/keys` | `wag_8a2b1c0d...` (40 hex) |
| Nama session | Admin; default `default` | `otp`, `notif`, `tutor` |
| Endpoint webhook (opsional) | Anda sediakan, HTTPS publik | `https://app.example.ac.id/wa/webhook` |

Simpan di env project Anda dengan nama yang sudah jadi konvensi:

```env
WA_GATEWAY_URL=https://wa.example.ac.id
WA_GATEWAY_API_KEY=wag_8a2b1c0d...
```

**Scope key yang perlu diminta** sesuai kebutuhan:

| Kebutuhan | Scope |
|---|---|
| Kirim pesan, normalisasi/cek nomor, resolve lid | `send` |
| Baca status session/QR, riwayat pesan, status kirim, progres bulk | `read` |
| Membuat/menghapus session, pairing, logout | `sessions` |
| Kelola API key & access log | `admin` (biasanya **tidak** diberikan ke aplikasi) |

Kombinasi paling umum untuk aplikasi: `send,read`.

---

## 2. Lima menit pertama

```bash
export WA_GATEWAY_URL=https://wa.example.ac.id
export WA_GATEWAY_API_KEY=wag_...

# 1) Gateway hidup?
curl "$WA_GATEWAY_URL/health"
# {"status":"ok"}

# 2) Key valid & session sudah login?
curl -H "X-API-Key: $WA_GATEWAY_API_KEY" "$WA_GATEWAY_URL/status"
# {"sessions":[{"name":"default","connected":true,"loggedIn":true,"jid":"628...@s.whatsapp.net",...}]}
#  → pastikan loggedIn:true untuk session yang akan Anda pakai

# 3) Kirim pesan pertama (ke nomor Anda sendiri)
curl -X POST "$WA_GATEWAY_URL/send/text" \
  -H "X-API-Key: $WA_GATEWAY_API_KEY" -H "Content-Type: application/json" \
  -d '{"session":"default","to":"628123456789","text":"Halo dari integrasi baru 👋"}'
# {"sent":true,"messageId":"3EB0..."}
```

Simpan `messageId` — itu kunci untuk melacak status kirim dan mengaitkan balasan.

Header auth boleh `X-API-Key: <key>` **atau** `Authorization: Bearer <key>`.

---

## 3. Aturan nomor tujuan

- Pakai format internasional **tanpa `+`**: `628123456789`.
- Nomor lokal `08...` hanya diterima bila gateway diset `DEFAULT_COUNTRY_CODE`; jangan
  mengandalkannya — **normalisasi di sisi gateway** dulu:

```bash
curl -X POST "$WA_GATEWAY_URL/normalize" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"phones":["0812-345-678","+62 813 9999 8888","abc"],"countryCode":"62"}'
# results[].normalized  → pakai ini sebagai "to"
# results[].error       → jangan dikirim, tampilkan ke user
```

- Grup: `to` = JID `...@g.us` (ambil dari `GET /groups`). Jika `isAnnounce:true`, hanya
  admin grup yang boleh mengirim.
- Sebelum kirim OTP/notifikasi penting, cek nomor aktif di WhatsApp (maks 250/request):

```bash
curl -X POST "$WA_GATEWAY_URL/check" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"phones":["628123456789"]}'
# results[].isOnWhatsApp:false → jangan kirim; tawarkan kanal lain (SMS/email)
```

---

## 4. Resep per kasus

### 4.1 OTP / kode verifikasi

```
normalize → check (opsional, hemat kuota) → send/text → simpan messageId
       → (opsional) POST /messages/status untuk tahu sudah delivered/read
```

```bash
curl -X POST "$WA_GATEWAY_URL/send/text" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"session":"otp","to":"628123456789","text":"Kode verifikasi Anda: 482913. Berlaku 5 menit. Jangan bagikan ke siapa pun."}'
```

Tips:
- Pakai **session terpisah** untuk OTP (mis. `otp`) agar nomor notifikasi massal yang
  berisiko diblokir tidak ikut menjatuhkan OTP.
- Set timeout di sisi Anda ≈ 30–60 s; gateway sendiri menunggu WhatsApp hingga 60 s.
- Jika respons `502`, WhatsApp menolak/timeout — tawarkan "kirim ulang" atau fallback SMS.
- Jangan kirim OTP ke nomor yang `isOnWhatsApp:false`.

### 4.2 Notifikasi massal (pengumuman, nilai, tagihan)

Jangan loop `send/text` dari aplikasi Anda — pakai **bulk** agar gateway yang mengatur
jeda anti-ban dan melanjutkan otomatis bila restart.

```bash
curl -X POST "$WA_GATEWAY_URL/send/bulk" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "session": "notif",
    "template": "Halo {{nama}}, nilai UAS {{mk}} Anda: {{nilai}}. Cek detail di portal.",
    "messages": [
      {"to":"628111","vars":{"nama":"Budi","mk":"Kalkulus","nilai":"A"}},
      {"to":"628222","vars":{"nama":"Sari","mk":"Kalkulus","nilai":"B+"}}
    ]
  }'
# 202 {"id":"fb49821e05e4e9de","status":"running","total":2,"sent":0,"failed":0,...}
```

Pantau progres (polling tiap 5–10 s cukup; 1000 penerima ≈ 1–2 jam dengan jeda default 3–6 s):

```bash
curl -H "X-API-Key: $WA_GATEWAY_API_KEY" "$WA_GATEWAY_URL/send/bulk/fb49821e05e4e9de"
# status: running | completed | cancelled | interrupted
# results[]: {to, status: pending|sent|failed, messageId?, error?}
```

Tips:
- Placeholder yang tidak ada di `vars` **dibiarkan** (`{{nama}}` tampil apa adanya) — validasi
  data Anda sebelum submit.
- `status:"interrupted"` = gateway restart saat job jalan; ia akan **melanjutkan sendiri**
  penerima `pending` saat start (bila admin mengaktifkan `BULK_AUTO_RESUME`, default aktif).
  Tidak perlu submit ulang — itu justru membuat duplikat.
- Pecah kiriman besar (>500) menjadi beberapa job pada waktu berbeda; kirim hanya ke
  penerima yang opt-in.
- Simpan `messageId` dari `results[]` bila Anda ingin menampilkan centang per penerima.

### 4.3 Chatbot / AI tutor (dua arah)

Anda perlu **webhook** (bagian 5). Alur:

```
user kirim pesan → gateway POST webhook {event:"message"} → aplikasi Anda memproses
       → balas via /send/text dengan replyTo (agar tampil sebagai kutipan)
```

```bash
# Balas sebagai quote ke pesan user (payload.id dari webhook)
curl -X POST "$WA_GATEWAY_URL/send/text" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "session": "tutor",
    "to": "628123456789",
    "text": "Turunan dari x² adalah 2x. Mau saya jelaskan langkahnya?",
    "replyTo": {"id":"3EB0ABCD1234","text":"apa turunan x^2?","fromMe":false}
  }'
```

Tips:
- Abaikan `payload.fromMe:true` (pesan yang dikirim gateway sendiri) agar bot tidak
  membalas dirinya sendiri.
- Di grup, `from` = JID grup, `sender` = pengirim. Balas ke `from` (grup) atau ke `sender`
  (japri) sesuai kebutuhan.
- `sender` bisa berbentuk `@lid` (alias privasi). Nomor asli ada di `senderAlt`; untuk lid
  lama yang tak punya `senderAlt`, panggil `POST /resolve-lid`.
- Untuk media masuk (`hasMedia:true`), gunakan `media.dataBase64` (bila admin mengaktifkan
  `DOWNLOAD_MEDIA`) atau, bila gateway menyimpan media, unduh via `GET /messages/{id}/media`.
- Balasan berupa gambar/dokumen/voice: `POST /send/image|file|voice` juga menerima `replyTo`.

### 4.4 Kirim media

```bash
# Gambar dari URL publik
curl -X POST "$WA_GATEWAY_URL/send/image" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"to":"628123456789","caption":"Kartu ujian","file":{"url":"https://app.example.ac.id/kartu/123.png"}}'

# Dokumen dari base64 (untuk file privat yang tidak boleh dibuka publik)
curl -X POST "$WA_GATEWAY_URL/send/file" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"to\":\"628123456789\",\"filename\":\"transkrip.pdf\",\"mimetype\":\"application/pdf\",\"file\":{\"base64\":\"$(base64 -i transkrip.pdf)\"}}"
```

- `file.url` harus bisa diakses **oleh server gateway** (bukan oleh browser user).
- Voice note: kirim OGG/Opus dengan `mimetype: "audio/ogg; codecs=opus"` agar tampil sebagai
  PTT, bukan file audio.

---

## 5. Menerima webhook

Webhook dikonfigurasi **oleh admin gateway** (`WEBHOOK_URL`, `WEBHOOK_EVENTS`) — satu URL
per instance gateway. Sampaikan URL Anda dan event yang dibutuhkan (`message`, `receipt`).

### Kontrak endpoint Anda

- Menerima `POST` JSON, balas **2xx secepat mungkin** (< 5 s). Proses berat (LLM, DB)
  lakukan di antrian/background. Gateway timeout 30 s dan menganggap non-2xx gagal.
- Gagal → gateway **retry** dengan backoff eksponensial (default 3× : 2 s, 4 s, 8 s), lalu
  menyerah. Jadi endpoint Anda **harus idempoten**: dedup berdasarkan `payload.id`
  (+ `session`).
- Verifikasi asal: gateway mengirim header `X-API-Key: <master key gateway>` bila admin
  menyetel `API_KEY`. Cocokkan dengan nilai yang diberikan admin; tolak selainnya. Pakai
  HTTPS.
- Satu URL menerima semua event — **dispatch berdasarkan `event`**.

### Payload `message`

```json
{
  "event": "message",
  "session": "tutor",
  "payload": {
    "id": "3EB0ABCD1234",
    "timestamp": 1717000000,
    "from": "628123456789@s.whatsapp.net",
    "sender": "628123456789@s.whatsapp.net",
    "senderAlt": "123456789012345@lid",
    "pushName": "Budi",
    "fromMe": false,
    "isGroup": false,
    "type": "text",
    "body": "apa turunan x^2?",
    "replyToId": "3EB0XYZ",
    "replyToText": "Materi hari ini: turunan",
    "hasMedia": false
  }
}
```

- `type`: `text | image | video | audio | document | sticker | unknown`.
- `replyToId`/`replyToText` hanya ada bila user membalas (quote) pesan lain — cocokkan
  dengan `messageId` yang pernah Anda kirim untuk tahu konteksnya.
- Media: `media: {mimetype, filename, fileLength, dataBase64?, error?}`.

### Payload `receipt` (centang)

```json
{
  "event": "receipt",
  "session": "notif",
  "payload": {
    "chat": "628123456789@s.whatsapp.net",
    "sender": "628123456789@s.whatsapp.net",
    "messageIds": ["3EB0ABCD1234"],
    "status": "read",
    "timestamp": 1717000005,
    "isGroup": false,
    "fromMe": false
  }
}
```

`status`: `delivered` (✓✓ abu) · `read` (✓✓ biru) · `played` (voice diputar) ·
`read-self`/`played-self` (Anda membaca pesan masuk dari device lain — abaikan untuk
pelacakan kiriman). Satu receipt bisa memuat banyak `messageIds`.

### Contoh receiver minimal (TypeScript / Express)

Contoh receiver untuk Go, NestJS, Laravel, dan Python (FastAPI) ada di [bagian 10](#10-contoh-klien-lengkap).

```typescript
import express from 'express';
const app = express();
app.use(express.json({ limit: '50mb' })); // media base64 bisa besar

const seen = new Set<string>(); // ganti dengan cache/DB di produksi

app.post('/wa/webhook', (req, res) => {
  if (req.header('x-api-key') !== process.env.WA_WEBHOOK_SECRET) return res.sendStatus(401);
  const { event, session, payload } = req.body;

  if (event === 'message') {
    const key = `${session}:${payload.id}`;
    if (seen.has(key) || payload.fromMe) return res.sendStatus(200);
    seen.add(key);
    queue.add('handle-message', { session, payload }); // proses di background
  } else if (event === 'receipt') {
    for (const id of payload.messageIds) queue.add('update-status', { id, status: payload.status });
  }
  res.sendStatus(200); // balas cepat
});
```

### Tidak punya endpoint publik?

Gunakan mode **pull**:
- Status kirim: `POST /messages/status` dengan daftar `ids` (maks 1000).
- Pesan masuk: `GET /messages?since=<unix>&order=asc&limit=1000` secara berkala, simpan
  `timestamp` terakhir sebagai cursor. Butuh admin mengaktifkan `STORE_MESSAGES`.

---

## 6. Status pengiriman

| Cara | Kapan dipakai |
|---|---|
| Webhook `receipt` | Real-time, UI chat/CRM |
| `POST /messages/status` | Batch/polling, laporan broadcast, tanpa endpoint publik |
| `GET /messages` (field `status`, `statusAt`) | Saat memuat ulang riwayat chat |

```bash
curl -X POST "$WA_GATEWAY_URL/messages/status" -H "X-API-Key: $WA_GATEWAY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"session":"notif","ids":["3EB0AAA","3EB0BBB"]}'
# results[] urut sama dengan ids → langsung zip dengan daftar penerima Anda
```

Ekspektasi yang benar:
- Status hanya **naik**: `sent → delivered → read → played`.
- `found:false` = id tidak ada di store (retensi habis, atau penyimpanan pesan belum aktif
  saat kirim). Bukan berarti gagal kirim.
- Penerima yang mematikan *read receipt* **tidak akan pernah** melewati `delivered`.
  Tampilkan sebagai "terkirim, tidak dikonfirmasi" — bukan "belum dibaca".

---

## 7. Menangani error & rate limit

| HTTP | Arti | Yang harus Anda lakukan |
|---|---|---|
| `400` | Body/field tidak valid, nomor salah format | Perbaiki request; jangan retry otomatis |
| `401` | Key kosong/salah | Cek env `WA_GATEWAY_API_KEY` |
| `403` | Scope kurang, key disabled/expired, batas session | Minta admin ubah key |
| `404` | Session/job/pesan tidak ada | Cek nama `session` |
| `409` | Session sudah ada / sudah login | Bukan error untuk alur kirim |
| `429` | Rate limit key terlampaui | Tunggu `Retry-After` detik, lalu retry |
| `501` | Fitur storage dimatikan (`/messages*`) | Minta admin aktifkan `STORE_MESSAGES` |
| `502` | WhatsApp menolak/timeout saat kirim | Retry dengan backoff (maks 2–3×), lalu fallback |

Semua error berbentuk `{"error":"..."}`. Saat `429`, gateway juga mengirim
`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` (unix) — gunakan untuk
throttling proaktif.

Pola retry yang disarankan untuk `/send/*` tunggal: hanya untuk `429`/`502`/timeout jaringan,
maksimal 3×, backoff 2 s → 4 s → 8 s. **Jangan** retry `400`/`403`. Untuk pengiriman massal,
gunakan `/send/bulk` dan biarkan gateway mengatur ulang.

---

## 8. Praktik baik (menghindari blokir nomor)

WA Gateway memakai protokol WhatsApp Web (tidak resmi) — nomor **bisa diblokir** bila
perilakunya terlihat seperti spam.

- Kirim hanya ke pengguna yang **opt-in** dan berharap menerima pesan dari Anda.
- Personalisasi (nama, konteks) lebih aman daripada teks identik massal.
- Gunakan `/send/bulk` untuk >10 penerima; jangan menurunkan `minDelayMs` di bawah default.
- Pisahkan nomor/session per fungsi: `otp` (kritis), `notif` (massal), `tutor` (interaktif).
- Jangan kirim ke nomor yang `isOnWhatsApp:false` berulang kali.
- Untuk OTP produksi berskala besar, pertimbangkan WhatsApp Cloud API resmi sebagai kanal
  utama dan gateway ini sebagai cadangan (atau sebaliknya).

---

## 9. Checklist go-live

- [ ] Key produksi terpisah dari dev, scope minimal (`send,read`), rate limit disetel
- [ ] `WA_GATEWAY_URL` memakai HTTPS
- [ ] Nomor dinormalisasi lewat `/normalize` sebelum disimpan/dikirim
- [ ] Session yang dipakai `loggedIn:true` — pantau `GET /status` di health check Anda
- [ ] Webhook: HTTPS, verifikasi `X-API-Key`, balas 2xx < 5 s, idempoten by `payload.id`
- [ ] `messageId` tersimpan di DB Anda untuk pelacakan status & threading balasan
- [ ] Penanganan `429` (`Retry-After`) dan `502` (fallback) sudah diuji
- [ ] Broadcast memakai `/send/bulk`, progres dipantau via `GET /send/bulk/{id}`
- [ ] Log aplikasi mencatat `messageId` + `session` untuk debugging bersama admin gateway
  (admin bisa mencocokkan dengan access log per key)

---

## 10. Contoh klien lengkap

Setiap contoh di bawah mencakup pola yang sama sehingga mudah dibandingkan:

1. **Klien** — `normalize`, `sendText` (dengan `replyTo` opsional), `sendBulk` + polling job,
   `messageStatus`, dan retry hanya untuk `429`/`502`/timeout (backoff 2 s → 4 s → 8 s).
2. **Webhook receiver** — verifikasi `X-API-Key`, dedup by `session:payload.id`, abaikan
   `fromMe`, balas 2xx segera, proses di background.

Env yang dipakai semua contoh: `WA_GATEWAY_URL`, `WA_GATEWAY_API_KEY`, dan
`WA_WEBHOOK_SECRET` (= master `API_KEY` gateway yang dikirim di header webhook).

Untuk daftar lengkap field/kode respons gunakan [`openapi.yaml`](../openapi.yaml) (bisa diimpor
ke Postman/Insomnia atau dipakai untuk generate client). Snippet ringkas untuk ditempel ke
`.github/copilot-instructions.md` ada di [`copilot-api.md`](copilot-api.md).

---

### 10.1 Go (net/http)

**Klien** — `internal/wagw/client.go`

```go
package wagw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

type Client struct {
	Base, APIKey string
	HTTP         *http.Client
}

func NewFromEnv() *Client {
	return &Client{
		Base:   os.Getenv("WA_GATEWAY_URL"),
		APIKey: os.Getenv("WA_GATEWAY_API_KEY"),
		HTTP:   &http.Client{Timeout: 90 * time.Second}, // /send/* bisa menunggu WA hingga 60 s
	}
}

// APIError membawa status HTTP + pesan {"error":"..."} dari gateway.
type APIError struct {
	Status     int
	Message    string
	RetryAfter time.Duration // terisi saat 429
}

func (e *APIError) Error() string { return fmt.Sprintf("wa-gateway %d: %s", e.Status, e.Message) }

func (e *APIError) Retryable() bool { return e.Status == 429 || e.Status == 502 }

// do mengirim request JSON dan retry maks 3x hanya untuk 429/502/timeout.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	backoff := 2 * time.Second
	for attempt := 1; ; attempt++ {
		err := c.once(ctx, method, path, body, out)
		if err == nil {
			return nil
		}
		var apiErr *APIError
		var netErr net.Error
		retry := (errors.As(err, &apiErr) && apiErr.Retryable()) || (errors.As(err, &netErr) && netErr.Timeout())
		if !retry || attempt >= 3 {
			return err
		}
		wait := backoff
		if apiErr != nil && apiErr.RetryAfter > 0 {
			wait = apiErr.RetryAfter
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct{ Error string `json:"error"` }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		apiErr := &APIError{Status: resp.StatusCode, Message: e.Error}
		if s, _ := strconv.Atoi(resp.Header.Get("Retry-After")); s > 0 {
			apiErr.RetryAfter = time.Duration(s) * time.Second
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- Tipe request/response ----

type ReplyTo struct {
	ID     string `json:"id"`
	Text   string `json:"text,omitempty"`
	FromMe bool   `json:"fromMe,omitempty"`
}

type BulkMessage struct {
	To   string            `json:"to"`
	Text string            `json:"text,omitempty"`
	Vars map[string]string `json:"vars,omitempty"`
}

type BulkJob struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // running|completed|cancelled|interrupted
	Total   int    `json:"total"`
	Sent    int    `json:"sent"`
	Failed  int    `json:"failed"`
	Results []struct {
		To        string `json:"to"`
		Status    string `json:"status"` // pending|sent|failed
		MessageID string `json:"messageId"`
		Error     string `json:"error"`
	} `json:"results"`
}

type MessageStatus struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // sent|delivered|read|played
	StatusAt int64  `json:"statusAt"`
	Found    bool   `json:"found"`
}

// ---- API ----

func (c *Client) Normalize(ctx context.Context, phone string) (string, error) {
	var out struct {
		Results []struct{ Normalized, Error string } `json:"results"`
	}
	if err := c.do(ctx, "POST", "/normalize", map[string]any{"phones": []string{phone}}, &out); err != nil {
		return "", err
	}
	if r := out.Results[0]; r.Error != "" {
		return "", fmt.Errorf("nomor tidak valid %q: %s", phone, r.Error)
	}
	return out.Results[0].Normalized, nil
}

func (c *Client) SendText(ctx context.Context, session, to, text string, reply *ReplyTo) (string, error) {
	var out struct{ MessageID string `json:"messageId"` }
	req := map[string]any{"session": session, "to": to, "text": text}
	if reply != nil {
		req["replyTo"] = reply
	}
	err := c.do(ctx, "POST", "/send/text", req, &out)
	return out.MessageID, err
}

func (c *Client) SendBulk(ctx context.Context, session, template string, msgs []BulkMessage) (*BulkJob, error) {
	var job BulkJob
	err := c.do(ctx, "POST", "/send/bulk", map[string]any{
		"session": session, "template": template, "messages": msgs,
	}, &job)
	return &job, err
}

func (c *Client) BulkJob(ctx context.Context, id string) (*BulkJob, error) {
	var job BulkJob
	err := c.do(ctx, "GET", "/send/bulk/"+id, nil, &job)
	return &job, err
}

// WaitBulk polling sampai job selesai (completed/cancelled/interrupted).
func (c *Client) WaitBulk(ctx context.Context, id string, every time.Duration) (*BulkJob, error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		job, err := c.BulkJob(ctx, id)
		if err != nil {
			return nil, err
		}
		if job.Status != "running" {
			return job, nil
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return job, ctx.Err()
		}
	}
}

func (c *Client) MessageStatus(ctx context.Context, session string, ids []string) ([]MessageStatus, error) {
	var out struct{ Results []MessageStatus `json:"results"` }
	err := c.do(ctx, "POST", "/messages/status", map[string]any{"session": session, "ids": ids}, &out)
	return out.Results, err
}
```

**Pemakaian**

```go
wa := wagw.NewFromEnv()
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
defer cancel()

// OTP
to, err := wa.Normalize(ctx, "0812-3456-789")
if err != nil { /* tampilkan ke user */ }
id, err := wa.SendText(ctx, "otp", to, "Kode verifikasi Anda: 482913", nil)

// Bulk + tunggu selesai
job, _ := wa.SendBulk(ctx, "notif", "Halo {{nama}}, nilai {{mk}}: {{nilai}}", []wagw.BulkMessage{
	{To: "628111", Vars: map[string]string{"nama": "Budi", "mk": "Kalkulus", "nilai": "A"}},
})
job, _ = wa.WaitBulk(ctx, job.ID, 10*time.Second)

// Status kirim
st, _ := wa.MessageStatus(ctx, "otp", []string{id})
```

**Webhook receiver** — `net/http` (cocok juga untuk chi/gin/echo)

```go
type envelope struct {
	Event   string          `json:"event"`
	Session string          `json:"session"`
	Payload json.RawMessage `json:"payload"`
}

type messagePayload struct {
	ID, From, Sender, SenderAlt, Type, Body, ReplyToID string
	FromMe, IsGroup                                     bool
	Timestamp                                           int64
}

type receiptPayload struct {
	MessageIDs []string `json:"messageIds"`
	Status     string   `json:"status"`
}

func webhookHandler(jobs chan<- any) http.HandlerFunc {
	secret := os.Getenv("WA_WEBHOOK_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && r.Header.Get("X-API-Key") != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var env envelope
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&env); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		switch env.Event {
		case "message":
			var p messagePayload
			_ = json.Unmarshal(env.Payload, &p)
			// dedup: simpan env.Session+":"+p.ID di cache/DB (SETNX Redis, atau UNIQUE index)
			if !p.FromMe && markSeen(env.Session+":"+p.ID) {
				jobs <- p // proses di goroutine/worker lain
			}
		case "receipt":
			var p receiptPayload
			_ = json.Unmarshal(env.Payload, &p)
			jobs <- p
		}
		w.WriteHeader(http.StatusOK) // balas cepat, sebelum proses berat
	}
}
```

---

### 10.2 Node.js / TypeScript

**Klien** — `src/wa-gateway.ts` (fetch bawaan Node ≥ 18, tanpa dependensi)

```typescript
export interface ReplyTo { id: string; text?: string; fromMe?: boolean }
export interface BulkMessage { to: string; text?: string; vars?: Record<string, string> }
export interface BulkJob {
  id: string; status: 'running' | 'completed' | 'cancelled' | 'interrupted';
  total: number; sent: number; failed: number;
  results: { to: string; status: 'pending' | 'sent' | 'failed'; messageId?: string; error?: string }[];
}
export interface MessageStatus { id: string; status?: 'sent'|'delivered'|'read'|'played'; statusAt?: number; found: boolean }

export class WaGatewayError extends Error {
  constructor(public status: number, message: string, public retryAfterSec = 0) { super(message); }
  get retryable() { return this.status === 429 || this.status === 502; }
}

const sleep = (ms: number) => new Promise(r => setTimeout(r, ms));

export class WaGateway {
  constructor(
    private base = process.env.WA_GATEWAY_URL!,
    private apiKey = process.env.WA_GATEWAY_API_KEY!,
  ) {}

  private async request<T>(method: string, path: string, body?: unknown, attempt = 1): Promise<T> {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 90_000);
    try {
      const res = await fetch(this.base + path, {
        method, signal: ctrl.signal,
        headers: { 'Content-Type': 'application/json', 'X-API-Key': this.apiKey },
        body: body ? JSON.stringify(body) : undefined,
      });
      if (!res.ok) {
        const { error } = await res.json().catch(() => ({ error: res.statusText }));
        throw new WaGatewayError(res.status, error, Number(res.headers.get('retry-after') ?? 0));
      }
      return (await res.json()) as T;
    } catch (err) {
      const retryable = (err instanceof WaGatewayError && err.retryable) || (err as Error).name === 'AbortError';
      if (!retryable || attempt >= 3) throw err;
      const wait = err instanceof WaGatewayError && err.retryAfterSec ? err.retryAfterSec * 1000 : 2000 * 2 ** (attempt - 1);
      await sleep(wait);
      return this.request<T>(method, path, body, attempt + 1);
    } finally {
      clearTimeout(timer);
    }
  }

  async normalize(phone: string): Promise<string> {
    const { results } = await this.request<{ results: { normalized?: string; error?: string }[] }>(
      'POST', '/normalize', { phones: [phone] });
    if (results[0].error) throw new Error(`nomor tidak valid "${phone}": ${results[0].error}`);
    return results[0].normalized!;
  }

  async sendText(session: string, to: string, text: string, replyTo?: ReplyTo): Promise<string> {
    const r = await this.request<{ messageId: string }>('POST', '/send/text', { session, to, text, replyTo });
    return r.messageId;
  }

  sendBulk(session: string, template: string, messages: BulkMessage[]) {
    return this.request<BulkJob>('POST', '/send/bulk', { session, template, messages });
  }

  bulkJob(id: string) { return this.request<BulkJob>('GET', `/send/bulk/${id}`); }

  async waitBulk(id: string, everyMs = 10_000): Promise<BulkJob> {
    for (;;) {
      const job = await this.bulkJob(id);
      if (job.status !== 'running') return job;
      await sleep(everyMs);
    }
  }

  async messageStatus(session: string, ids: string[]): Promise<MessageStatus[]> {
    const r = await this.request<{ results: MessageStatus[] }>('POST', '/messages/status', { session, ids });
    return r.results;
  }
}
```

**Pemakaian**

```typescript
const wa = new WaGateway();
const to = await wa.normalize('0812-3456-789');
const messageId = await wa.sendText('otp', to, 'Kode verifikasi Anda: 482913');

const job = await wa.sendBulk('notif', 'Halo {{nama}}, nilai {{mk}}: {{nilai}}', [
  { to: '628111', vars: { nama: 'Budi', mk: 'Kalkulus', nilai: 'A' } },
]);
const done = await wa.waitBulk(job.id);
console.log(done.sent, done.failed);
```

**Webhook receiver — Express**

```typescript
import express from 'express';

const app = express();
app.use(express.json({ limit: '50mb' })); // media base64 bisa besar

app.post('/wa/webhook', (req, res) => {
  if (req.header('x-api-key') !== process.env.WA_WEBHOOK_SECRET) return res.sendStatus(401);
  const { event, session, payload } = req.body;

  if (event === 'message' && !payload.fromMe) {
    // dedup: SETNX di Redis / UNIQUE(session,id) di DB; contoh in-memory hanya untuk dev
    if (markSeen(`${session}:${payload.id}`)) queue.add('wa-message', { session, payload });
  } else if (event === 'receipt') {
    queue.add('wa-receipt', { ids: payload.messageIds, status: payload.status });
  }
  res.sendStatus(200); // balas cepat
});
```

**Webhook receiver — NestJS**

```typescript
// wa-webhook.controller.ts
import { Body, Controller, Headers, HttpCode, Post, UnauthorizedException } from '@nestjs/common';
import { InjectQueue } from '@nestjs/bullmq';
import { Queue } from 'bullmq';

interface Envelope { event: 'message' | 'receipt'; session: string; payload: any }

@Controller('wa')
export class WaWebhookController {
  constructor(@InjectQueue('wa') private readonly queue: Queue) {}

  @Post('webhook')
  @HttpCode(200)
  async handle(@Headers('x-api-key') key: string, @Body() body: Envelope) {
    if (key !== process.env.WA_WEBHOOK_SECRET) throw new UnauthorizedException();
    const { event, session, payload } = body;

    if (event === 'message' && !payload.fromMe) {
      // jobId unik = dedup gratis dari BullMQ (job dgn id sama diabaikan)
      await this.queue.add('message', { session, payload }, { jobId: `${session}:${payload.id}` });
    } else if (event === 'receipt') {
      await this.queue.add('receipt', { ids: payload.messageIds, status: payload.status });
    }
    return { ok: true };
  }
}

// wa-gateway.service.ts — bungkus klien di atas sebagai provider
import { Injectable } from '@nestjs/common';
import { WaGateway } from './wa-gateway';

@Injectable()
export class WaGatewayService extends WaGateway {}
```

Di `main.ts` naikkan batas body untuk media base64:
`app.use(json({ limit: '50mb' }))` (dari `express`).

---

### 10.3 PHP / Laravel

**Klien** — `app/Services/WaGateway.php` (Laravel HTTP client)

```php
<?php

namespace App\Services;

use Illuminate\Http\Client\RequestException;
use Illuminate\Http\Client\Response;
use Illuminate\Support\Facades\Http;

class WaGateway
{
    public function __construct(
        private readonly string $base = '',
        private readonly string $apiKey = '',
    ) {}

    public static function fromConfig(): self
    {
        return new self(config('services.wa_gateway.url'), config('services.wa_gateway.key'));
    }

    private function http()
    {
        return Http::baseUrl($this->base)
            ->withHeaders(['X-API-Key' => $this->apiKey])
            ->acceptJson()
            ->timeout(90)
            // retry hanya utk 429/502/timeout; hormati Retry-After bila ada
            ->retry(3, function (int $attempt, \Throwable $e) {
                if ($e instanceof RequestException && $e->response->status() === 429) {
                    $ra = (int) $e->response->header('Retry-After');
                    if ($ra > 0) return $ra * 1000;
                }
                return 2000 * (2 ** ($attempt - 1));
            }, function (\Throwable $e) {
                if ($e instanceof RequestException) {
                    return in_array($e->response->status(), [429, 502], true);
                }
                return true; // connection/timeout
            })
            ->throw(); // non-2xx → RequestException; pesan ada di ->response->json('error')
    }

    public function normalize(string $phone): string
    {
        $r = $this->http()->post('/normalize', ['phones' => [$phone]])->json('results.0');
        if (!empty($r['error'])) {
            throw new \InvalidArgumentException("Nomor tidak valid \"$phone\": {$r['error']}");
        }
        return $r['normalized'];
    }

    /** @param array{id:string,text?:string,fromMe?:bool}|null $replyTo */
    public function sendText(string $session, string $to, string $text, ?array $replyTo = null): string
    {
        return $this->http()
            ->post('/send/text', array_filter(compact('session', 'to', 'text', 'replyTo')))
            ->json('messageId');
    }

    /** @param array<int, array{to:string,text?:string,vars?:array<string,string>}> $messages */
    public function sendBulk(string $session, string $template, array $messages): array
    {
        return $this->http()->post('/send/bulk', compact('session', 'template', 'messages'))->json();
    }

    public function bulkJob(string $id): array
    {
        return $this->http()->get("/send/bulk/$id")->json();
    }

    public function waitBulk(string $id, int $everySec = 10): array
    {
        while (true) {
            $job = $this->bulkJob($id);
            if ($job['status'] !== 'running') return $job;
            sleep($everySec);
        }
    }

    public function messageStatus(string $session, array $ids): array
    {
        return $this->http()->post('/messages/status', compact('session', 'ids'))->json('results');
    }
}
```

`config/services.php` + `.env`:

```php
'wa_gateway' => [
    'url' => env('WA_GATEWAY_URL', 'http://localhost:3111'),
    'key' => env('WA_GATEWAY_API_KEY'),
    'webhook_secret' => env('WA_WEBHOOK_SECRET'),
],
```

**Pemakaian** (di controller/job)

```php
$wa = WaGateway::fromConfig();
$to = $wa->normalize($request->input('phone'));
$messageId = $wa->sendText('otp', $to, "Kode verifikasi Anda: $otp");

$job = $wa->sendBulk('notif', 'Halo {{nama}}, nilai {{mk}}: {{nilai}}', [
    ['to' => '628111', 'vars' => ['nama' => 'Budi', 'mk' => 'Kalkulus', 'nilai' => 'A']],
]);
// jangan blok request HTTP — dispatch job Laravel yang memanggil waitBulk / bulkJob
```

**Webhook receiver** — controller + queued job

```php
<?php
// routes/api.php
Route::post('/wa/webhook', [\App\Http\Controllers\WaWebhookController::class, 'handle'])
    ->withoutMiddleware([\App\Http\Middleware\VerifyCsrfToken::class]);

// app/Http/Controllers/WaWebhookController.php
namespace App\Http\Controllers;

use App\Jobs\HandleWaMessage;
use App\Jobs\HandleWaReceipt;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Cache;

class WaWebhookController extends Controller
{
    public function handle(Request $request)
    {
        if (!hash_equals((string) config('services.wa_gateway.webhook_secret'), (string) $request->header('X-API-Key'))) {
            abort(401);
        }

        $event   = $request->input('event');
        $session = $request->input('session');
        $payload = $request->input('payload', []);

        if ($event === 'message' && empty($payload['fromMe'])) {
            // dedup 24 jam: Cache::add hanya sukses bila key belum ada (atomik di Redis)
            if (Cache::add("wa:seen:$session:{$payload['id']}", 1, now()->addDay())) {
                HandleWaMessage::dispatch($session, $payload);
            }
        } elseif ($event === 'receipt') {
            HandleWaReceipt::dispatch($payload['messageIds'] ?? [], $payload['status'] ?? '');
        }

        return response()->json(['ok' => true]); // balas cepat
    }
}
```

Naikkan `post_max_size`/`upload_max_filesize` PHP (≥ 50M) bila gateway mengirim media base64.

---

### 10.4 Python

**Klien** — `wa_gateway.py` (`requests`)

```python
import os
import time
from dataclasses import dataclass
from typing import Any

import requests


class WaGatewayError(Exception):
    def __init__(self, status: int, message: str, retry_after: int = 0):
        super().__init__(f"wa-gateway {status}: {message}")
        self.status, self.retry_after = status, retry_after

    @property
    def retryable(self) -> bool:
        return self.status in (429, 502)


@dataclass
class WaGateway:
    base: str = os.getenv("WA_GATEWAY_URL", "http://localhost:3111")
    api_key: str = os.getenv("WA_GATEWAY_API_KEY", "")
    timeout: int = 90  # /send/* bisa menunggu WA hingga 60 s

    def _request(self, method: str, path: str, json: Any = None) -> Any:
        backoff = 2.0
        for attempt in range(1, 4):
            try:
                r = requests.request(
                    method, self.base + path, json=json, timeout=self.timeout,
                    headers={"X-API-Key": self.api_key, "Content-Type": "application/json"},
                )
                if r.status_code >= 400:
                    msg = (r.json() if r.content else {}).get("error", r.reason)
                    raise WaGatewayError(r.status_code, msg, int(r.headers.get("Retry-After", 0)))
                return r.json()
            except (WaGatewayError, requests.Timeout, requests.ConnectionError) as e:
                retryable = e.retryable if isinstance(e, WaGatewayError) else True
                if not retryable or attempt == 3:
                    raise
                wait = e.retry_after if isinstance(e, WaGatewayError) and e.retry_after else backoff
                time.sleep(wait)
                backoff *= 2

    # ---- API ----

    def normalize(self, phone: str) -> str:
        res = self._request("POST", "/normalize", {"phones": [phone]})["results"][0]
        if "error" in res:
            raise ValueError(f"nomor tidak valid {phone!r}: {res['error']}")
        return res["normalized"]

    def send_text(self, session: str, to: str, text: str, reply_to: dict | None = None) -> str:
        body: dict[str, Any] = {"session": session, "to": to, "text": text}
        if reply_to:
            body["replyTo"] = reply_to  # {"id": ..., "text": ..., "fromMe": False}
        return self._request("POST", "/send/text", body)["messageId"]

    def send_bulk(self, session: str, template: str, messages: list[dict]) -> dict:
        return self._request("POST", "/send/bulk",
                             {"session": session, "template": template, "messages": messages})

    def bulk_job(self, job_id: str) -> dict:
        return self._request("GET", f"/send/bulk/{job_id}")

    def wait_bulk(self, job_id: str, every: float = 10) -> dict:
        while True:
            job = self.bulk_job(job_id)
            if job["status"] != "running":
                return job
            time.sleep(every)

    def message_status(self, session: str, ids: list[str]) -> list[dict]:
        return self._request("POST", "/messages/status", {"session": session, "ids": ids})["results"]
```

**Pemakaian**

```python
wa = WaGateway()
to = wa.normalize("0812-3456-789")
message_id = wa.send_text("otp", to, "Kode verifikasi Anda: 482913")

job = wa.send_bulk("notif", "Halo {{nama}}, nilai {{mk}}: {{nilai}}", [
    {"to": "628111", "vars": {"nama": "Budi", "mk": "Kalkulus", "nilai": "A"}},
])
done = wa.wait_bulk(job["id"])          # di produksi: jalankan di Celery/RQ, bukan di request
print(done["sent"], done["failed"])

for s in wa.message_status("otp", [message_id]):
    print(s["id"], s.get("status"), s["found"])
```

**Webhook receiver — FastAPI**

```python
import os
from fastapi import BackgroundTasks, FastAPI, Header, HTTPException, Request

app = FastAPI()
SECRET = os.getenv("WA_WEBHOOK_SECRET", "")


@app.post("/wa/webhook")
async def wa_webhook(
    request: Request,
    tasks: BackgroundTasks,
    x_api_key: str | None = Header(default=None),
):
    if SECRET and x_api_key != SECRET:
        raise HTTPException(401)
    body = await request.json()
    event, session, payload = body.get("event"), body.get("session"), body.get("payload", {})

    if event == "message" and not payload.get("fromMe"):
        # dedup: redis.set(name, 1, nx=True, ex=86400) atau UNIQUE(session,id) di DB
        if mark_seen(f"{session}:{payload['id']}"):
            tasks.add_task(handle_message, session, payload)  # atau Celery .delay()
    elif event == "receipt":
        tasks.add_task(update_status, payload.get("messageIds", []), payload.get("status"))

    return {"ok": True}  # balas cepat; proses berat berjalan setelah respons
```

**Webhook receiver — Django** (view fungsi, tanpa CSRF)

```python
import json, os
from django.http import HttpResponse, JsonResponse
from django.views.decorators.csrf import csrf_exempt
from django.views.decorators.http import require_POST

@csrf_exempt
@require_POST
def wa_webhook(request):
    if request.headers.get("X-API-Key") != os.getenv("WA_WEBHOOK_SECRET"):
        return HttpResponse(status=401)
    body = json.loads(request.body)
    event, session, payload = body.get("event"), body.get("session"), body.get("payload", {})
    if event == "message" and not payload.get("fromMe"):
        if mark_seen(f"{session}:{payload['id']}"):
            handle_message.delay(session, payload)  # Celery task
    elif event == "receipt":
        update_status.delay(payload.get("messageIds", []), payload.get("status"))
    return JsonResponse({"ok": True})
```

Set `DATA_UPLOAD_MAX_MEMORY_SIZE` (Django) atau batas body server (uvicorn/nginx
`client_max_body_size 50m`) bila gateway mengirim media base64.
