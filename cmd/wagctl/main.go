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
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
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
    keys create [flags]               Buat API key baru
    keys get <id>                     Detail satu key
    keys update <id> [flags]          Update atribut key
    keys rotate <id>                  Rotate secret key
    keys delete <id>                  Hapus key

  Operasi Gateway:
    status [--session=<n>]            Status koneksi session
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
		baseURL = "http://localhost:3111"
	}

	apiKey := *keyFlag
	if apiKey == "" {
		apiKey = os.Getenv("WA_GATEWAY_API_KEY")
	}

	c := newClient(baseURL, apiKey)
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "keys":
		runKeys(c, rest)
	case "status":
		runStatus(c, rest)
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
		fmt.Fprintf(os.Stderr, "Gunakan: wagctl keys <list|create|get|update|rotate|delete>\n")
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
		fmt.Print("Penggunaan: wagctl keys create [flags]\n")
		fs.PrintDefaults()
	}
	name := fs.String("name", "", "Nama key (wajib)")
	scopes := fs.String("scopes", "*", "Scope: send,read,sessions,admin,* (pisah koma)")
	rateLimit := fs.Int("rate-limit", 0, "Maks request per window; 0 = unlimited")
	rateWindow := fs.Int("rate-window", 60, "Panjang window rate limit (detik)")
	maxSessions := fs.Int("max-sessions", 0, "Batas session/device; 0 = unlimited")
	expiresAt := fs.Int64("expires-at", 0, "Expiry Unix timestamp; 0 = tidak kedaluwarsa")
	_ = fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name wajib diisi")
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
		var k struct {
			Secret string `json:"secret"`
		}
		_ = json.Unmarshal(data, &k)
		if k.Secret != "" {
			fmt.Fprintf(os.Stderr, "\n⚠️  Simpan secret berikut — hanya muncul SEKALI:\n   %s\n\n", k.Secret)
		}
	}
	printJSON(data, code)
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
		var k struct {
			Secret string `json:"secret"`
		}
		_ = json.Unmarshal(data, &k)
		if k.Secret != "" {
			fmt.Fprintf(os.Stderr, "\n⚠️  Secret BARU — hanya muncul sekali:\n   %s\n\n", k.Secret)
		}
	}
	printJSON(data, code)
}

func cmdKeysDelete(c *client, args []string) {
	fs := flag.NewFlagSet("keys delete", flag.ExitOnError)
	fs.Usage = func() { fmt.Println("Penggunaan: wagctl keys delete <id>") }
	force := fs.Bool("force", false, "Hapus tanpa konfirmasi")
	_ = fs.Parse(args)

	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "error: id diperlukan")
		os.Exit(2)
	}

	if !*force {
		fmt.Printf("Hapus key %q? Ketik 'ya' untuk konfirmasi: ", id)
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "ya" {
			fmt.Println("Dibatalkan.")
			return
		}
	}

	data, code, err := c.do("DELETE", "/admin/keys/"+id, nil)
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

// Pastikan strconv diimport (dipakai di cmdKeysUpdate).
var _ = strconv.ParseBool
