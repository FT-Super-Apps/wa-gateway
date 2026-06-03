package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"wa-gateway/internal/config"
	"wa-gateway/internal/gateway"
)

// Server exposes the gateway over a REST API.
type Server struct {
	cfg *config.Config
	mgr *gateway.Manager
}

// New creates a new API server.
func New(cfg *config.Config, mgr *gateway.Manager) *Server {
	return &Server{cfg: cfg, mgr: mgr}
}

// Handler builds the HTTP handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.auth(s.handleStatus))
	mux.HandleFunc("GET /qr", s.auth(s.handleQR))
	mux.HandleFunc("POST /pair", s.auth(s.handlePair))
	mux.HandleFunc("GET /groups", s.auth(s.handleListGroups))
	mux.HandleFunc("GET /messages", s.auth(s.handleListMessages))

	mux.HandleFunc("GET /sessions", s.auth(s.handleListSessions))
	mux.HandleFunc("POST /sessions", s.auth(s.handleCreateSession))
	mux.HandleFunc("DELETE /sessions/{name}", s.auth(s.handleDeleteSession))

	mux.HandleFunc("POST /send/text", s.auth(s.handleSendText))
	mux.HandleFunc("POST /send/image", s.auth(s.handleSendImage))
	mux.HandleFunc("POST /send/file", s.auth(s.handleSendFile))
	mux.HandleFunc("POST /send/voice", s.auth(s.handleSendVoice))
	mux.HandleFunc("POST /send/bulk", s.auth(s.handleSendBulk))
	mux.HandleFunc("GET /send/bulk", s.auth(s.handleListBulk))
	mux.HandleFunc("GET /send/bulk/{id}", s.auth(s.handleBulkStatus))
	mux.HandleFunc("POST /check", s.auth(s.handleCheckPhones))
	mux.HandleFunc("POST /logout", s.auth(s.handleLogout))
	return mux
}

// auth wraps a handler with API-key checking when an API key is configured.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey != "" && r.Header.Get("X-API-Key") != s.cfg.APIKey {
			writeError(w, http.StatusUnauthorized, "invalid or missing API key")
			return
		}
		next(w, r)
	}
}

// sessionName resolves the target session from query param, defaulting to "default".
func sessionName(r *http.Request) string {
	if s := r.URL.Query().Get("session"); s != "" {
		return s
	}
	return "default"
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleStatus returns one session's status (?session=) or all sessions.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if name := r.URL.Query().Get("session"); name != "" {
		sess, err := s.mgr.Get(name)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sess.Status())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.mgr.List()})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.mgr.List()})
}

type createSessionRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "field 'name' is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	sess, err := s.mgr.Create(ctx, req.Name)
	if err != nil {
		if errors.Is(err, gateway.ErrSessionExists) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess.Status())
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "session name is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.mgr.Remove(ctx, name); err != nil {
		if errors.Is(err, gateway.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}

// handleListMessages returns stored message history (requires STORE_MESSAGES=true).
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	if !s.mgr.StorageEnabled() {
		writeError(w, http.StatusNotImplemented, "message storage is disabled; set STORE_MESSAGES=true to enable")
		return
	}
	q := gateway.MessageQuery{
		Session: r.URL.Query().Get("session"),
		Chat:    r.URL.Query().Get("chat"),
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.Before = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	msgs, err := s.mgr.Messages(ctx, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "count": len(msgs)})
}

// handleListGroups lists the groups joined by a session's account.
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(sessionName(r), w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	groups, err := sess.ListGroups(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	sess, err := s.mgr.Get(sessionName(r))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	code := sess.CurrentQR()
	if code == "" {
		if sess.Status().LoggedIn {
			writeError(w, http.StatusConflict, "already logged in, no QR needed")
			return
		}
		writeError(w, http.StatusNotFound, "no QR code available yet, try again shortly")
		return
	}

	png, err := qrcode.Encode(code, qrcode.Medium, 512)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to render QR: "+err.Error())
		return
	}

	if r.URL.Query().Get("format") == "png" {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"code":      code,
		"pngBase64": base64.StdEncoding.EncodeToString(png),
	})
}

type pairRequest struct {
	Session string `json:"session"`
	Phone   string `json:"phone"`
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Phone == "" {
		writeError(w, http.StatusBadRequest, "phone is required")
		return
	}

	name := req.Session
	if name == "" {
		name = "default"
	}
	sess, err := s.mgr.Get(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	code, err := sess.PairPhone(r.Context(), req.Phone)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"code":  code,
		"phone": req.Phone,
		"hint":  "WhatsApp > Linked Devices > Link with phone number instead",
	})
}

type sendTextRequest struct {
	Session string `json:"session"`
	To      string `json:"to"`
	Text    string `json:"text"`
}

func (s *Server) handleSendText(w http.ResponseWriter, r *http.Request) {
	var req sendTextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.To == "" || req.Text == "" {
		writeError(w, http.StatusBadRequest, "fields 'to' and 'text' are required")
		return
	}
	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	id, err := sess.SendText(ctx, req.To, req.Text)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "messageId": id})
}

type fileSource struct {
	URL    string `json:"url"`
	Base64 string `json:"base64"`
}

type sendMediaRequest struct {
	Session  string     `json:"session"`
	To       string     `json:"to"`
	Caption  string     `json:"caption"`
	Mimetype string     `json:"mimetype"`
	Filename string     `json:"filename"`
	Seconds  uint32     `json:"seconds"`
	File     fileSource `json:"file"`
}

func (s *Server) resolveMedia(ctx context.Context, req sendMediaRequest) (gateway.MediaInput, error) {
	var data []byte
	switch {
	case req.File.Base64 != "":
		b, err := base64.StdEncoding.DecodeString(req.File.Base64)
		if err != nil {
			return gateway.MediaInput{}, fmt.Errorf("invalid base64: %w", err)
		}
		data = b
	case req.File.URL != "":
		b, err := fetchURL(ctx, req.File.URL)
		if err != nil {
			return gateway.MediaInput{}, err
		}
		data = b
	default:
		return gateway.MediaInput{}, fmt.Errorf("file.url or file.base64 is required")
	}

	return gateway.MediaInput{
		Data:     data,
		Mimetype: req.Mimetype,
		Filename: req.Filename,
		Caption:  req.Caption,
		Seconds:  req.Seconds,
	}, nil
}

func (s *Server) handleSendImage(w http.ResponseWriter, r *http.Request) {
	s.handleSendMedia(w, r, func(sess *gateway.Session) sendMediaFunc { return sess.SendImage })
}

func (s *Server) handleSendFile(w http.ResponseWriter, r *http.Request) {
	s.handleSendMedia(w, r, func(sess *gateway.Session) sendMediaFunc { return sess.SendFile })
}

func (s *Server) handleSendVoice(w http.ResponseWriter, r *http.Request) {
	s.handleSendMedia(w, r, func(sess *gateway.Session) sendMediaFunc { return sess.SendVoice })
}

type sendMediaFunc func(ctx context.Context, to string, in gateway.MediaInput) (string, error)

func (s *Server) handleSendMedia(w http.ResponseWriter, r *http.Request, pick func(*gateway.Session) sendMediaFunc) {
	var req sendMediaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "field 'to' is required")
		return
	}
	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	in, err := s.resolveMedia(ctx, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id, err := pick(sess)(ctx, req.To, in)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "messageId": id})
}

type checkRequest struct {
	Session string   `json:"session"`
	Phones  []string `json:"phones"`
}

// handleCheckPhones memeriksa apakah nomor-nomor terdaftar di WhatsApp.
func (s *Server) handleCheckPhones(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Phones) == 0 {
		writeError(w, http.StatusBadRequest, "field 'phones' is required and must not be empty")
		return
	}
	if len(req.Phones) > 250 {
		writeError(w, http.StatusBadRequest, "maximum 250 numbers per request")
		return
	}

	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	results, err := sess.CheckPhones(ctx, req.Phones)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results)})
}

type logoutRequest struct {
	Session string `json:"session"`
}

// handleSendBulk starts a mass-send job and returns its job ID immediately.
func (s *Server) handleSendBulk(w http.ResponseWriter, r *http.Request) {
	var req gateway.BulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	job, err := s.mgr.SubmitBulk(req)
	if err != nil {
		if errors.Is(err, gateway.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// handleBulkStatus returns the progress/results of a bulk job.
func (s *Server) handleBulkStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.mgr.BulkJob(id)
	if !ok {
		writeError(w, http.StatusNotFound, "bulk job not found (it may have expired)")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleListBulk lists known bulk jobs (newest first).
func (s *Server) handleListBulk(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.mgr.BulkJobs()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := sess.Logout(ctx); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"loggedOut": true})
}

// session resolves a session by name (defaulting to "default") and writes a 404
// error response if not found. The bool reports success.
func (s *Server) session(name string, w http.ResponseWriter) (*gateway.Session, bool) {
	if name == "" {
		name = "default"
	}
	sess, err := s.mgr.Get(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return nil, false
	}
	return sess, true
}

func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch url: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch url: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
