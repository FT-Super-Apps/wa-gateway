# Access Log Monitoring

wa-gateway mencatat setiap request API yang terautentikasi ke tabel SQLite
`gw_access_logs`. Fitur ini membantu memantau penggunaan API key, mendeteksi
penyalahgunaan, dan melacak pola traffic.

---

## Cara Mengaktifkan

Set env var `ACCESS_LOG_RETENTION_DAYS` ke angka positif (default sudah `7`):

```env
ACCESS_LOG_RETENTION_DAYS=7   # simpan 7 hari, lalu hapus otomatis
ACCESS_LOG_RETENTION_DAYS=30  # simpan 30 hari
ACCESS_LOG_RETENTION_DAYS=0   # nonaktif — tidak ada yang dicatat
```

Restart service setelah mengubah nilai ini.

---

## Endpoint API

### `GET /admin/logs` — Semua log

```http
GET /admin/logs HTTP/1.1
X-API-Key: <master-key-atau-key-scope-admin>
```

**Query parameters:**

| Parameter | Tipe    | Default | Keterangan |
|-----------|---------|---------|------------|
| `key`     | string  | —       | Filter berdasarkan key ID |
| `since`   | integer | —       | Unix timestamp; tampilkan log ≥ waktu ini |
| `limit`   | integer | `100`   | Jumlah entri (maks `1000`) |

**Contoh:**

```bash
# 100 entri terbaru
curl -H "X-API-Key: $MASTER_KEY" http://localhost:3111/admin/logs

# Semua log untuk key tertentu, 500 entri
curl -H "X-API-Key: $MASTER_KEY" \
  "http://localhost:3111/admin/logs?key=key_a1b2c3d4&limit=500"

# Log sejak 24 jam lalu (Linux)
SINCE=$(date -d '24 hours ago' +%s)
curl -H "X-API-Key: $MASTER_KEY" \
  "http://localhost:3111/admin/logs?since=$SINCE"

# Log sejak 24 jam lalu (macOS)
SINCE=$(date -v-24H +%s)
curl -H "X-API-Key: $MASTER_KEY" \
  "http://localhost:3111/admin/logs?since=$SINCE"
```

---

### `GET /admin/keys/{id}/logs` — Log satu key

Shortcut untuk melihat log satu key spesifik.

```http
GET /admin/keys/key_a1b2c3d4/logs?limit=100 HTTP/1.1
X-API-Key: <master-key-atau-key-scope-admin>
```

**Query parameters:** sama seperti `/admin/logs` (tanpa `key`).

```bash
curl -H "X-API-Key: $MASTER_KEY" \
  "http://localhost:3111/admin/keys/key_a1b2c3d4/logs?limit=50"
```

---

## Response Format

```json
{
  "count": 3,
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
    },
    {
      "id": 40,
      "keyId": "key_a1b2c3d4",
      "keyName": "app-otp",
      "method": "GET",
      "path": "/status",
      "statusCode": 200,
      "latencyMs": 5,
      "ip": "10.0.0.5",
      "createdAt": 1717459190
    }
  ]
}
```

### Field `AccessLogEntry`

| Field        | Tipe    | Keterangan |
|--------------|---------|------------|
| `id`         | integer | ID auto-increment (urutan insert ke DB) |
| `keyId`      | string  | ID API key; `"master"` jika tanpa auth |
| `keyName`    | string  | Nama API key saat request terjadi |
| `method`     | string  | HTTP method: `GET`, `POST`, `PATCH`, dll |
| `path`       | string  | Path URL tanpa query string |
| `statusCode` | integer | HTTP status code response |
| `latencyMs`  | integer | Waktu proses request dalam milidetik |
| `ip`         | string  | IP client (cek `X-Forwarded-For` lalu `RemoteAddr`) |
| `createdAt`  | integer | Unix timestamp saat request terjadi |

Hasil selalu diurutkan **terbaru dulu** (`createdAt DESC`).

---

## Yang Dicatat vs Tidak

| Kondisi | Dicatat? |
|---------|----------|
| Request berhasil (2xx) | ✅ |
| Request kena rate limit (429) | ✅ |
| Request dengan scope tidak cukup (403) | ✅ |
| Request tanpa `API_KEY` env (mode tanpa auth) | ✅ (keyId = `"master"`) |
| Request dengan API key salah/tidak valid (401) | ❌ |
| Request dengan key disabled/expired (403) | ❌ |
| `ACCESS_LOG_RETENTION_DAYS=0` | ❌ (semua nonaktif) |

---

## Cara Kerja Internal

```
Request masuk
    │
    ▼
[auth middleware]
    ├── Autentikasi gagal (401/403) → langsung reject, tidak dicatat
    └── Autentikasi berhasil
            │
            ▼
       [handler] ← responseWriter wrapper capture status code
            │
            ▼
       Record(AccessLogEntry) → buffer in-memory
            │
            ▼ (tiap 5 detik)
       flush ke SQLite gw_access_logs
            │
            ▼ (tiap 24 jam / saat startup)
       purge entri > ACCESS_LOG_RETENTION_DAYS hari
```

**Performa:** Log di-buffer di memory dan di-flush ke DB tiap 5 detik —
tidak ada I/O sinkron per-request, sehingga tidak menambah latency.

---

## Schema Database

```sql
CREATE TABLE IF NOT EXISTS gw_access_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    key_id      TEXT    NOT NULL DEFAULT '',
    key_name    TEXT    NOT NULL DEFAULT '',
    method      TEXT    NOT NULL DEFAULT '',
    path        TEXT    NOT NULL DEFAULT '',
    status_code INTEGER NOT NULL DEFAULT 0,
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    ip          TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_gw_access_logs_key ON gw_access_logs(key_id);
CREATE INDEX IF NOT EXISTS idx_gw_access_logs_ts  ON gw_access_logs(created_at);
```

File SQLite: `<STORE_DIR>/gateway.db` (default `./data/gateway.db`).

Untuk query langsung (debug/analisis):

```bash
sqlite3 data/gateway.db \
  "SELECT key_name, COUNT(*) as req, AVG(latency_ms) as avg_ms
   FROM gw_access_logs
   WHERE created_at > strftime('%s','now','-1 day')
   GROUP BY key_name
   ORDER BY req DESC;"
```

---

## Integrasi TypeScript

```typescript
const WA_URL = process.env.WA_GATEWAY_URL ?? 'http://localhost:3111';
const WA_KEY = process.env.WA_GATEWAY_API_KEY ?? '';
const headers = { 'Content-Type': 'application/json', 'X-API-Key': WA_KEY };

interface AccessLogEntry {
  id: number;
  keyId: string;
  keyName: string;
  method: string;
  path: string;
  statusCode: number;
  latencyMs: number;
  ip: string;
  createdAt: number;
}

// Ambil 100 log terbaru untuk satu key
async function getKeyLogs(keyId: string, limit = 100): Promise<AccessLogEntry[]> {
  const res = await fetch(
    `${WA_URL}/admin/keys/${keyId}/logs?limit=${limit}`,
    { headers }
  );
  if (!res.ok) throw new Error((await res.json()).error);
  const data = await res.json();
  return data.logs ?? [];
}

// Cek apakah ada lonjakan error 5xx dalam 1 jam terakhir
async function checkErrorSpike(keyId: string): Promise<boolean> {
  const since = Math.floor(Date.now() / 1000) - 3600; // 1 jam
  const res = await fetch(
    `${WA_URL}/admin/keys/${keyId}/logs?since=${since}&limit=1000`,
    { headers }
  );
  const data = await res.json();
  const logs: AccessLogEntry[] = data.logs ?? [];
  const errors = logs.filter(l => l.statusCode >= 500).length;
  return errors > 10; // spike jika > 10 error 5xx dalam 1 jam
}
```

---

## Integrasi Python

```python
import os, requests, time

WA_URL = os.getenv("WA_GATEWAY_URL", "http://localhost:3111")
HEADERS = {
    "Content-Type": "application/json",
    "X-API-Key": os.getenv("WA_GATEWAY_API_KEY", ""),
}

def get_logs(key_id: str | None = None, since: int | None = None, limit: int = 100):
    """Ambil access log. key_id=None untuk semua key."""
    if key_id:
        url = f"{WA_URL}/admin/keys/{key_id}/logs"
    else:
        url = f"{WA_URL}/admin/logs"
    
    params = {"limit": limit}
    if since:
        params["since"] = since
    if key_id and not key_id:
        params["key"] = key_id

    r = requests.get(url, headers=HEADERS, params=params)
    r.raise_for_status()
    return r.json().get("logs", [])

# Contoh: laporan penggunaan per key hari ini
def daily_report():
    since = int(time.time()) - 86400  # 24 jam lalu
    logs = get_logs(since=since, limit=1000)
    
    from collections import Counter
    by_key = Counter(l["keyName"] for l in logs)
    errors = Counter(l["keyName"] for l in logs if l["statusCode"] >= 400)
    
    print("=== Laporan Harian ===")
    for key, total in by_key.most_common():
        err = errors.get(key, 0)
        print(f"  {key}: {total} request, {err} error ({err/total*100:.1f}%)")

daily_report()
```
