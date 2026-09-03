// wagctl adalah CLI untuk wa-gateway: kelola API key, kirim pesan, cek status.
//
// Konfigurasi via env:
//
//	WA_GATEWAY_URL     base URL gateway (default http://localhost:3111)
//	WA_GATEWAY_API_KEY API key (master atau managed key dengan scope yang sesuai)
//
// Atau via flag global --url dan --key.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
)

// ---- config & client -------------------------------------------------------

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(baseURL, apiKey string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) do(method, path string, body any) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

// printJSON mencetak JSON dengan indentasi ke stdout.
func printJSON(data []byte, statusCode int) {
	if statusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			fmt.Fprintf(os.Stderr, "error: %s\n", e.Error)
			os.Exit(1)
		}
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		fmt.Println(string(data))
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// ---- usage -----------------------------------------------------------------

const usageText = `wagctl — WA Gateway CLI

Usage:
  wagctl [--url=<url>] [--key=<key>] <perintah> [args...]

Flag global:
  --url   Base URL gateway (default: $WA_GATEWAY_URL atau http://localhost:3111)
  --key   API key         (default: $WA_GATEWAY_API_KEY)

Perintah tersedia:

  API Key Management (butuh scope admin / master key):
    keys list                         Daftar semua API key
    keys create [flags]               Buat API key baru (tanpa flag = interaktif)
    keys create -i                    Buat API key baru mode interaktif (tanya-jawab)
    keys get <id>                     Detail satu key
    keys update <id> [flags]          Update atribut key
    keys enable <id>                  Aktifkan key
    keys disable <id>                 Nonaktifkan key
    keys rotate <id>                  Rotate secret key
    keys delete <id> [--force]        Hapus key (--force lewati konfirmasi)

  Operasi Gateway:
    status [--session=<n>]            Status koneksi session
    qr [--session=<n>] [--watch]      Tampilkan QR pairing di terminal
    pair --phone=<p> [--session=<n>]  Minta kode pairing 8-digit
    check  --phones=<p1,p2,...>       Cek nomor di WhatsApp
    normalize --phones=<p1,p2,...>    Normalisasi nomor telepon
    send text --to=<phone> --text=<t> Kirim pesan teks

Jalankan wagctl <perintah> --help untuk flag spesifik.
`

// ---- main ------------------------------------------------------------------

func main() {
	globalFlags := flag.NewFlagSet("wagctl", flag.ContinueOnError)
	globalFlags.Usage = func() { fmt.Print(usageText) }

	urlFlag := globalFlags.String("url", "", "Base URL wa-gateway")
	keyFlag := globalFlags.String("key", "", "API key")

	if err := globalFlags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	args := globalFlags.Args()
	if len(args) == 0 {
		fmt.Print(usageText)
		os.Exit(0)
	}

	baseURL := *urlFlag
	if baseURL == "" {
		baseURL = os.Getenv("WA_GATEWAY_URL")
	}
	if baseURL == "" {
		// Fallback saat dijalankan di dalam container gateway: pakai PORT
		// milik server (di-set lewat .env) sehingga cukup
		// `docker exec wa-gateway /app/wagctl ...` tanpa set env tambahan.
		if port := os.Getenv("PORT"); port != "" {
			baseURL = "http://localhost:" + port
		}
	}
	if baseURL == "" {
		baseURL = "http://localhost:3111"
	}

	apiKey := *keyFlag
	if apiKey == "" {
		apiKey = os.Getenv("WA_GATEWAY_API_KEY")
	}
	if apiKey == "" {
		// Fallback ke API_KEY server (tersedia di dalam container).
		apiKey = os.Getenv("API_KEY")
	}

	c := newClient(baseURL, apiKey)
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "keys":
		runKeys(c, rest)
	case "status":
		runStatus(c, rest)
	case "qr":
		runQR(c, rest)
	case "pair":
		runPair(c, rest)
	case "check":
		runCheck(c, rest)
	case "normalize":
		runNormalize(c, rest)
	case "send":
		runSend(c, rest)
	default:
		fmt.Fprintf(os.Stderr, "perintah tidak dikenal: %q\nJalankan wagctl --help untuk bantuan.\n", cmd)
		os.Exit(2)
	}
}

// ---- keys ------------------------------------------------------------------

func runKeys(c *client, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Gunakan: wagctl keys <list|create|get|update|enable|disable|rotate|delete>\n")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		cmdKeysList(c, rest)
	case "create":
		cmdKeysCreate(c, rest)
	case "get":
		cmdKeysGet(c, rest)
	case "update":
		cmdKeysUpdate(c, rest)
	case "enable":
		cmdKeysToggle(c, rest, true)
	case "disable":
		cmdKeysToggle(c, rest, false)
	case "rotate":
		cmdKeysRotate(c, rest)
	case "delete", "del", "rm":
		cmdKeysDelete(c, rest)
	default:
		fmt.Fprintf(os.Stderr, "sub-perintah keys tidak dikenal: %q\n", sub)
		os.Exit(2)
	}
}

func cmdKeysList(c *client, args []string) {
	fs := flag.NewFlagSet("keys list", flag.ExitOnError)
	fs.Usage = func() { fmt.Println("Penggunaan: wagctl keys list\n\nDaftar semua API key (tanpa secret).") }
	_ = fs.Parse(args)

	data, code, err := c.do("GET", "/admin/keys", nil)
	fatalOnErr(err)
	printJSON(data, code)
}

func cmdKeysCreate(c *client, args []string) {
	fs := flag.NewFlagSet("keys create", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl keys create [flags]\n\n" +
			"Tanpa flag apa pun akan masuk mode interaktif (tanya-jawab).\n" +
			"Paksa mode interaktif dengan -i / --interactive.\n\n")
		fs.PrintDefaults()
	}
	interactive := fs.Bool("interactive", false, "Mode interaktif (tanya-jawab)")
	iShort := fs.Bool("i", false, "Alias --interactive")
	name := fs.String("name", "", "Nama key (wajib di mode non-interaktif)")
	scopes := fs.String("scopes", "*", "Scope: send,read,sessions,admin,* (pisah koma)")
	rateLimit := fs.Int("rate-limit", 0, "Maks request per window; 0 = unlimited")
	rateWindow := fs.Int("rate-window", 60, "Panjang window rate limit (detik)")
	maxSessions := fs.Int("max-sessions", 0, "Batas session/device; 0 = unlimited")
	expiresAt := fs.Int64("expires-at", 0, "Expiry Unix timestamp; 0 = tidak kedaluwarsa")
	_ = fs.Parse(args)

	// Tanpa argumen sama sekali, atau -i/--interactive → mode tanya-jawab.
	if *interactive || *iShort || len(args) == 0 {
		cmdKeysCreateInteractive(c)
		return
	}

	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name wajib diisi (atau jalankan tanpa flag untuk mode interaktif)")
		os.Exit(2)
	}

	body := map[string]any{
		"name":          *name,
		"scopes":        strings.Split(*scopes, ","),
		"rateLimit":     *rateLimit,
		"rateWindowSec": *rateWindow,
		"maxSessions":   *maxSessions,
		"expiresAt":     *expiresAt,
	}
	data, code, err := c.do("POST", "/admin/keys", body)
	fatalOnErr(err)

	if code == 201 {
		announceSecret(c.baseURL, data, "Access key dibuat. Simpan API Key sekarang — tidak bisa dilihat lagi.")
	} else {
		printJSON(data, code)
	}
}

// cmdKeysCreateInteractive memandu pembuatan key lewat tanya-jawab.
func cmdKeysCreateInteractive(c *client) {
	r := bufio.NewReader(os.Stdin)
	fmt.Println("=== Buat API Key Baru (interaktif) ===")
	fmt.Println("Tekan Enter untuk memakai nilai default di dalam [ ].")
	fmt.Println()

	var name string
	for {
		name = promptLine(r, "Nama key (wajib)", "")
		if name != "" {
			break
		}
		fmt.Fprintln(os.Stderr, "  ⚠️  Nama tidak boleh kosong.")
	}

	fmt.Println()
	fmt.Println("Scope tersedia:")
	fmt.Println("  send      kirim pesan, normalize, check")
	fmt.Println("  read      status, qr, groups, messages")
	fmt.Println("  sessions  pair, logout, kelola session")
	fmt.Println("  admin     kelola API key")
	fmt.Println("  *         semua akses")
	scopes := promptLine(r, "Scope (pisah koma)", "*")

	fmt.Println()
	rateLimit := promptInt(r, "Maks request per window (0 = unlimited)", 0)
	rateWindow := 60
	if rateLimit > 0 {
		rateWindow = promptInt(r, "Panjang window (detik)", 60)
	}
	maxSessions := promptInt(r, "Batas session/device (0 = unlimited)", 0)
	days := promptInt(r, "Kedaluwarsa dalam hari (0 = tidak pernah)", 0)

	var expiresAt int64
	if days > 0 {
		expiresAt = time.Now().AddDate(0, 0, days).Unix()
	}

	fmt.Println()
	fmt.Println("=== Ringkasan ===")
	fmt.Printf("  Nama         : %s\n", name)
	fmt.Printf("  Scope        : %s\n", scopes)
	fmt.Printf("  Rate limit   : %s\n", describeRate(rateLimit, rateWindow))
	fmt.Printf("  Max session  : %s\n", zeroUnlimited(maxSessions))
	fmt.Printf("  Kedaluwarsa  : %s\n", describeExpiry(days, expiresAt))
	fmt.Println()

	if !isYes(promptLine(r, "Buat key ini? (ya/tidak)", "ya")) {
		fmt.Println("Dibatalkan.")
		return
	}

	body := map[string]any{
		"name":          name,
		"scopes":        splitTrim(scopes),
		"rateLimit":     rateLimit,
		"rateWindowSec": rateWindow,
		"maxSessions":   maxSessions,
		"expiresAt":     expiresAt,
	}
	data, code, err := c.do("POST", "/admin/keys", body)
	fatalOnErr(err)

	if code == 201 {
		announceSecret(c.baseURL, data, "Access key dibuat. Simpan API Key sekarang — tidak bisa dilihat lagi.")
	} else {
		printJSON(data, code)
	}
}

func cmdKeysGet(c *client, args []string) {
	fs := flag.NewFlagSet("keys get", flag.ExitOnError)
	fs.Usage = func() { fmt.Println("Penggunaan: wagctl keys get <id>") }
	_ = fs.Parse(args)

	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "error: id diperlukan")
		os.Exit(2)
	}
	data, code, err := c.do("GET", "/admin/keys/"+id, nil)
	fatalOnErr(err)
	printJSON(data, code)
}

func cmdKeysUpdate(c *client, args []string) {
	fs := flag.NewFlagSet("keys update", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl keys update <id> [flags]\n")
		fs.PrintDefaults()
	}
	// Sentinel: -1 = tidak di-set untuk int, "" = tidak di-set untuk string
	name := fs.String("name", "", "Nama baru (kosong = tidak diubah)")
	scopes := fs.String("scopes", "", "Scope baru, pisah koma (kosong = tidak diubah)")
	rateLimit := fs.Int("rate-limit", -1, "Rate limit baru; -1 = tidak diubah")
	rateWindow := fs.Int("rate-window", -1, "Rate window baru (detik); -1 = tidak diubah")
	maxSessions := fs.Int("max-sessions", -1, "Max sessions baru; -1 = tidak diubah")
	enabled := fs.String("enabled", "", "true/false (kosong = tidak diubah)")
	expiresAt := fs.Int64("expires-at", -1, "Expiry unix timestamp; -1 = tidak diubah")
	_ = fs.Parse(args)

	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "error: id diperlukan")
		os.Exit(2)
	}

	body := map[string]any{}
	if *name != "" {
		body["name"] = *name
	}
	if *scopes != "" {
		body["scopes"] = strings.Split(*scopes, ",")
	}
	if *rateLimit != -1 {
		body["rateLimit"] = *rateLimit
	}
	if *rateWindow != -1 {
		body["rateWindowSec"] = *rateWindow
	}
	if *maxSessions != -1 {
		body["maxSessions"] = *maxSessions
	}
	if *enabled != "" {
		b, err := strconv.ParseBool(*enabled)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: --enabled harus true atau false")
			os.Exit(2)
		}
		body["enabled"] = b
	}
	if *expiresAt != -1 {
		body["expiresAt"] = *expiresAt
	}

	if len(body) == 0 {
		fmt.Fprintln(os.Stderr, "error: tidak ada field yang diubah — tambahkan flag seperti --enabled=false")
		os.Exit(2)
	}

	data, code, err := c.do("PATCH", "/admin/keys/"+id, body)
	fatalOnErr(err)
	printJSON(data, code)
}

func cmdKeysRotate(c *client, args []string) {
	fs := flag.NewFlagSet("keys rotate", flag.ExitOnError)
	fs.Usage = func() { fmt.Println("Penggunaan: wagctl keys rotate <id>") }
	_ = fs.Parse(args)

	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "error: id diperlukan")
		os.Exit(2)
	}
	data, code, err := c.do("POST", "/admin/keys/"+id+"/rotate", nil)
	fatalOnErr(err)

	if code == 200 {
		announceSecret(c.baseURL, data, "API Key di-rotate. Simpan API Key baru — yang lama langsung nonaktif.")
	} else {
		printJSON(data, code)
	}
}

func cmdKeysDelete(c *client, args []string) {
	fs := flag.NewFlagSet("keys delete", flag.ExitOnError)
	fs.Usage = func() { fmt.Println("Penggunaan: wagctl keys delete [--force] <id>") }
	force := fs.Bool("force", false, "Hapus tanpa konfirmasi")
	fs.BoolVar(force, "f", false, "Alias --force")
	// Pisahkan flag dari <id> agar --force tetap dikenali walau ditulis setelah id.
	flags, positional := splitFlagsAndPositional(args)
	_ = fs.Parse(flags)

	id := ""
	if len(positional) > 0 {
		id = positional[0]
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "error: id diperlukan")
		os.Exit(2)
	}

	if !*force {
		fmt.Printf("Hapus key %q? Ketik 'ya' untuk konfirmasi: ", id)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintln(os.Stderr, "\nerror: konfirmasi butuh input interaktif.")
			fmt.Fprintln(os.Stderr, "  → tambahkan --force, atau jalankan dengan stdin: docker exec -i ...")
			os.Exit(2)
		}
		if !isYes(line) {
			fmt.Println("Dibatalkan.")
			return
		}
	}

	data, code, err := c.do("DELETE", "/admin/keys/"+id, nil)
	fatalOnErr(err)
	printJSON(data, code)
}

func cmdKeysToggle(c *client, args []string, enable bool) {
	action := "enable"
	if !enable {
		action = "disable"
	}
	fs := flag.NewFlagSet("keys "+action, flag.ExitOnError)
	fs.Usage = func() { fmt.Printf("Penggunaan: wagctl keys %s <id>\n", action) }
	_ = fs.Parse(args)

	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "error: id diperlukan")
		os.Exit(2)
	}
	data, code, err := c.do("POST", "/admin/keys/"+id+"/"+action, nil)
	fatalOnErr(err)
	printJSON(data, code)
}

// ---- status ----------------------------------------------------------------

func runStatus(c *client, args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	session := fs.String("session", "", "Nama session (kosong = semua)")
	_ = fs.Parse(args)

	path := "/status"
	if *session != "" {
		path += "?session=" + *session
	}
	data, code, err := c.do("GET", path, nil)
	fatalOnErr(err)
	printJSON(data, code)
}

// ---- qr / pairing ----------------------------------------------------------

type qrResponse struct {
	Code      string `json:"code"`
	PngBase64 string `json:"pngBase64"`
	Error     string `json:"error"`
}

func runQR(c *client, args []string) {
	fs := flag.NewFlagSet("qr", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl qr [flags]\n\n" +
			"Menampilkan QR code untuk pairing WhatsApp di terminal (bisa langsung discan).\n\n")
		fs.PrintDefaults()
	}
	session := fs.String("session", "default", "Nama session")
	pngFile := fs.String("png", "", "Simpan QR sebagai file PNG ke path ini")
	raw := fs.Bool("raw", false, "Cetak hanya string kode QR (tanpa render ASCII)")
	watch := fs.Bool("watch", false, "Pantau & render ulang otomatis sampai QR siap / login")
	_ = fs.Parse(args)

	path := "/qr?session=" + *session
	lastCode := ""
	for {
		data, code, err := c.do("GET", path, nil)
		fatalOnErr(err)

		var resp qrResponse
		_ = json.Unmarshal(data, &resp)

		switch {
		case code == 200 && resp.Code != "":
			if resp.Code != lastCode {
				lastCode = resp.Code
				renderQR(resp, *raw, *pngFile, *session)
			}
			if !*watch {
				return
			}
		case code == http.StatusConflict:
			fmt.Println("✅ Session sudah login — tidak perlu QR.")
			return
		case code == http.StatusNotFound:
			if !*watch {
				fmt.Fprintf(os.Stderr, "error: %s\n", orDefault(resp.Error, "QR belum tersedia, coba lagi sebentar"))
				os.Exit(1)
			}
			fmt.Fprint(os.Stderr, "\rMenunggu QR tersedia... ")
		default:
			fmt.Fprintf(os.Stderr, "error: %s\n", orDefault(resp.Error, fmt.Sprintf("HTTP %d", code)))
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
	}
}

// renderQR menampilkan QR ke terminal (ASCII), opsional simpan PNG, atau cetak raw.
func renderQR(resp qrResponse, raw bool, pngFile, session string) {
	if pngFile != "" {
		png, err := base64.StdEncoding.DecodeString(resp.PngBase64)
		if err == nil && len(png) > 0 {
			if werr := os.WriteFile(pngFile, png, 0o644); werr != nil {
				fmt.Fprintf(os.Stderr, "warning: gagal simpan PNG: %v\n", werr)
			} else {
				fmt.Printf("📁 QR PNG disimpan ke %s\n", pngFile)
			}
		}
	}

	if raw {
		fmt.Println(resp.Code)
		return
	}

	q, err := qrcode.New(resp.Code, qrcode.Medium)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: gagal render QR: %v\n", err)
		fmt.Println("Kode QR mentah:")
		fmt.Println(resp.Code)
		return
	}
	fmt.Printf("\n📱 Scan QR ini di WhatsApp (session: %s)\n", session)
	fmt.Println("   WhatsApp > Perangkat Tertaut > Tautkan Perangkat")
	fmt.Println(q.ToSmallString(false))
}

func runPair(c *client, args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl pair --phone=<nomor> [flags]\n\n" +
			"Meminta kode pairing 8-digit (alternatif QR untuk server headless).\n\n")
		fs.PrintDefaults()
	}
	phone := fs.String("phone", "", "Nomor WhatsApp yang akan dipasangkan (wajib)")
	session := fs.String("session", "default", "Nama session")
	_ = fs.Parse(args)

	if *phone == "" {
		fmt.Fprintln(os.Stderr, "error: --phone wajib diisi")
		os.Exit(2)
	}

	body := map[string]string{"session": *session, "phone": *phone}
	data, code, err := c.do("POST", "/pair", body)
	fatalOnErr(err)

	if code != 200 {
		printJSON(data, code)
		os.Exit(1)
	}

	var resp struct {
		Code  string `json:"code"`
		Phone string `json:"phone"`
		Hint  string `json:"hint"`
	}
	if json.Unmarshal(data, &resp) != nil || resp.Code == "" {
		printJSON(data, code)
		return
	}

	fmt.Printf("\n🔑 Kode pairing untuk %s (session: %s):\n\n", resp.Phone, *session)
	fmt.Printf("        %s\n\n", resp.Code)
	fmt.Println("Masukkan di HP: WhatsApp > Perangkat Tertaut > Tautkan Perangkat")
	fmt.Println("> Tautkan dengan nomor telepon")
	if resp.Hint != "" {
		fmt.Printf("(%s)\n", resp.Hint)
	}
}

// ---- check -----------------------------------------------------------------

func runCheck(c *client, args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl check [flags]\n")
		fs.PrintDefaults()
	}
	phones := fs.String("phones", "", "Nomor telepon (pisah koma, wajib)")
	session := fs.String("session", "default", "Nama session")
	_ = fs.Parse(args)

	if *phones == "" {
		fmt.Fprintln(os.Stderr, "error: --phones wajib diisi")
		os.Exit(2)
	}

	list := strings.Split(*phones, ",")
	for i, p := range list {
		list[i] = strings.TrimSpace(p)
	}
	body := map[string]any{"session": *session, "phones": list}
	data, code, err := c.do("POST", "/check", body)
	fatalOnErr(err)
	printJSON(data, code)
}

// ---- normalize -------------------------------------------------------------

func runNormalize(c *client, args []string) {
	fs := flag.NewFlagSet("normalize", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl normalize [flags]\n")
		fs.PrintDefaults()
	}
	phones := fs.String("phones", "", "Nomor telepon (pisah koma, wajib)")
	cc := fs.String("country-code", "", "Kode negara, mis. 62 (default dari DEFAULT_COUNTRY_CODE gateway)")
	_ = fs.Parse(args)

	if *phones == "" {
		fmt.Fprintln(os.Stderr, "error: --phones wajib diisi")
		os.Exit(2)
	}

	list := strings.Split(*phones, ",")
	for i, p := range list {
		list[i] = strings.TrimSpace(p)
	}
	body := map[string]any{"phones": list}
	if *cc != "" {
		body["countryCode"] = *cc
	}
	data, code, err := c.do("POST", "/normalize", body)
	fatalOnErr(err)
	printJSON(data, code)
}

// ---- send ------------------------------------------------------------------

func runSend(c *client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Gunakan: wagctl send <text|image|file>")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "text":
		cmdSendText(c, rest)
	case "image":
		cmdSendImage(c, rest)
	case "file":
		cmdSendFile(c, rest)
	default:
		fmt.Fprintf(os.Stderr, "sub-perintah send tidak dikenal: %q\n", sub)
		os.Exit(2)
	}
}

func cmdSendText(c *client, args []string) {
	fs := flag.NewFlagSet("send text", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl send text [flags]\n")
		fs.PrintDefaults()
	}
	to := fs.String("to", "", "Nomor tujuan atau group JID (wajib)")
	text := fs.String("text", "", "Teks pesan (wajib)")
	session := fs.String("session", "default", "Nama session")
	_ = fs.Parse(args)

	if *to == "" || *text == "" {
		fmt.Fprintln(os.Stderr, "error: --to dan --text wajib diisi")
		os.Exit(2)
	}
	body := map[string]string{"session": *session, "to": *to, "text": *text}
	data, code, err := c.do("POST", "/send/text", body)
	fatalOnErr(err)
	printJSON(data, code)
}

func cmdSendImage(c *client, args []string) {
	fs := flag.NewFlagSet("send image", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl send image [flags]\n")
		fs.PrintDefaults()
	}
	to := fs.String("to", "", "Nomor tujuan (wajib)")
	url := fs.String("url", "", "URL gambar")
	caption := fs.String("caption", "", "Keterangan gambar")
	session := fs.String("session", "default", "Nama session")
	_ = fs.Parse(args)

	if *to == "" || *url == "" {
		fmt.Fprintln(os.Stderr, "error: --to dan --url wajib diisi")
		os.Exit(2)
	}
	body := map[string]any{
		"session": *session, "to": *to,
		"caption": *caption,
		"file":    map[string]string{"url": *url},
	}
	data, code, err := c.do("POST", "/send/image", body)
	fatalOnErr(err)
	printJSON(data, code)
}

func cmdSendFile(c *client, args []string) {
	fs := flag.NewFlagSet("send file", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Print("Penggunaan: wagctl send file [flags]\n")
		fs.PrintDefaults()
	}
	to := fs.String("to", "", "Nomor tujuan (wajib)")
	url := fs.String("url", "", "URL file (wajib)")
	filename := fs.String("filename", "", "Nama file yang ditampilkan")
	mimetype := fs.String("mimetype", "application/octet-stream", "MIME type file")
	session := fs.String("session", "default", "Nama session")
	_ = fs.Parse(args)

	if *to == "" || *url == "" {
		fmt.Fprintln(os.Stderr, "error: --to dan --url wajib diisi")
		os.Exit(2)
	}
	body := map[string]any{
		"session": *session, "to": *to,
		"filename": *filename, "mimetype": *mimetype,
		"file": map[string]string{"url": *url},
	}
	data, code, err := c.do("POST", "/send/file", body)
	fatalOnErr(err)
	printJSON(data, code)
}

// ---- helpers ---------------------------------------------------------------

func fatalOnErr(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// orDefault mengembalikan s bila tidak kosong, jika tidak kembalikan def.
func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// announceSecret mencetak kredensial sekali dengan format gaya sicekcok-go:
// baris pembuka, blok Label/API Key, header wajib, contoh JSON header & curl.
func announceSecret(baseURL string, data []byte, headline string) {
	var k struct {
		Secret string `json:"secret"`
		Name   string `json:"name"`
	}
	_ = json.Unmarshal(data, &k)
	if k.Secret == "" {
		return
	}
	if baseURL == "" {
		baseURL = "http://localhost:3000"
	}
	out := os.Stdout
	fmt.Fprintf(out, "%s\n\n", headline)
	fmt.Fprintf(out, "  Label:    %s\n", labelOrDash(k.Name))
	fmt.Fprintf(out, "  API Key:  %s\n\n", k.Secret)
	fmt.Fprintln(out, "Kirim salah satu header berikut di setiap request:")
	fmt.Fprintf(out, "  X-API-Key: %s\n", k.Secret)
	fmt.Fprintf(out, "  Authorization: Bearer %s\n\n", k.Secret)
	fmt.Fprintln(out, "Contoh JSON header:")
	fmt.Fprintln(out, "{")
	fmt.Fprintf(out, "  \"X-API-Key\": \"%s\"\n", k.Secret)
	fmt.Fprintln(out, "}")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Contoh curl (kirim teks):")
	fmt.Fprintf(out, "curl -X POST %s/send/text \\\n", baseURL)
	fmt.Fprintf(out, "  -H 'X-API-Key: %s' \\\n", k.Secret)
	fmt.Fprintln(out, "  -H 'Content-Type: application/json' \\")
	fmt.Fprintln(out, "  -d '{\"session\":\"default\",\"to\":\"628xxxxxxxxxx\",\"text\":\"Halo\"}'")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Atau pakai Bearer: -H 'Authorization: Bearer %s'\n", k.Secret)
}

// labelOrDash mengembalikan "-" bila label kosong.
func labelOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// promptLine menampilkan label + default lalu membaca satu baris input.
// Enter kosong mengembalikan def. Keluar dengan pesan jelas bila stdin EOF
// (mis. `docker exec` tanpa -it) agar tidak loop pada field wajib.
func promptLine(r *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(os.Stderr, "\nerror: mode interaktif butuh input terminal — jalankan dengan `docker exec -it ...`")
		os.Exit(2)
	}
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}

// promptInt membaca input angka, mengulang bila tidak valid.
func promptInt(r *bufio.Reader, label string, def int) int {
	for {
		n, err := strconv.Atoi(promptLine(r, label, strconv.Itoa(def)))
		if err != nil {
			fmt.Fprintln(os.Stderr, "  ⚠️  Masukkan angka yang valid.")
			continue
		}
		return n
	}
}

// isYes melaporkan apakah jawaban bermakna "ya".
func isYes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ya", "y", "yes":
		return true
	}
	return false
}

// splitTrim memecah string dipisah koma dan membuang spasi/entri kosong.
func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitFlagsAndPositional memisahkan flag (diawali '-') dari argumen posisional,
// sehingga flag boolean tetap dikenali walau ditulis setelah argumen (mis.
// `keys delete <id> --force`).
func splitFlagsAndPositional(args []string) (flags, positional []string) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			positional = append(positional, a)
		}
	}
	return flags, positional
}

// zeroUnlimited memformat 0/negatif sebagai "unlimited".
func zeroUnlimited(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// describeRate memformat rate limit untuk ringkasan.
func describeRate(limit, window int) string {
	if limit <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d request / %d detik", limit, window)
}

// describeExpiry memformat masa berlaku untuk ringkasan.
func describeExpiry(days int, ts int64) string {
	if days <= 0 || ts <= 0 {
		return "tidak pernah"
	}
	return fmt.Sprintf("%d hari (%s)", days, time.Unix(ts, 0).Format("2006-01-02 15:04"))
}

// Pastikan strconv diimport (dipakai di cmdKeysUpdate).
var _ = strconv.ParseBool
