package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	mux.HandleFunc("GET /status", s.auth(gateway.ScopeRead, s.handleStatus))
	mux.HandleFunc("GET /qr", s.auth(gateway.ScopeRead, s.handleQR))
	mux.HandleFunc("POST /pair", s.auth(gateway.ScopeSessions, s.handlePair))
	mux.HandleFunc("GET /groups", s.auth(gateway.ScopeRead, s.handleListGroups))
	mux.HandleFunc("GET /messages", s.auth(gateway.ScopeRead, s.handleListMessages))
	mux.HandleFunc("GET /messages/{id}/media", s.auth(gateway.ScopeRead, s.handleGetMedia))

	mux.HandleFunc("GET /sessions", s.auth(gateway.ScopeRead, s.handleListSessions))
	mux.HandleFunc("POST /sessions", s.auth(gateway.ScopeSessions, s.handleCreateSession))
	mux.HandleFunc("DELETE /sessions/{name}", s.auth(gateway.ScopeSessions, s.handleDeleteSession))

	mux.HandleFunc("POST /send/text", s.auth(gateway.ScopeSend, s.handleSendText))
	mux.HandleFunc("POST /send/image", s.auth(gateway.ScopeSend, s.handleSendImage))
	mux.HandleFunc("POST /send/file", s.auth(gateway.ScopeSend, s.handleSendFile))
	mux.HandleFunc("POST /send/voice", s.auth(gateway.ScopeSend, s.handleSendVoice))
	mux.HandleFunc("POST /send/bulk", s.auth(gateway.ScopeSend, s.handleSendBulk))
	mux.HandleFunc("GET /send/bulk", s.auth(gateway.ScopeRead, s.handleListBulk))
	mux.HandleFunc("GET /send/bulk/{id}", s.auth(gateway.ScopeRead, s.handleBulkStatus))
	mux.HandleFunc("POST /normalize", s.auth(gateway.ScopeSend, s.handleNormalize))
	mux.HandleFunc("POST /check", s.auth(gateway.ScopeSend, s.handleCheckPhones))
	mux.HandleFunc("POST /resolve-lid", s.auth(gateway.ScopeSend, s.handleResolveLID))
	mux.HandleFunc("POST /logout", s.auth(gateway.ScopeSessions, s.handleLogout))

	// API key management (butuh scope admin / master key).
	mux.HandleFunc("POST /admin/keys", s.auth(gateway.ScopeAdmin, s.handleCreateKey))
	mux.HandleFunc("GET /admin/keys", s.auth(gateway.ScopeAdmin, s.handleListKeys))
	mux.HandleFunc("GET /admin/keys/{id}", s.auth(gateway.ScopeAdmin, s.handleGetKey))
	mux.HandleFunc("PATCH /admin/keys/{id}", s.auth(gateway.ScopeAdmin, s.handleUpdateKey))
	mux.HandleFunc("POST /admin/keys/{id}/rotate", s.auth(gateway.ScopeAdmin, s.handleRotateKey))
	mux.HandleFunc("POST /admin/keys/{id}/enable", s.auth(gateway.ScopeAdmin, s.handleEnableKey))
	mux.HandleFunc("POST /admin/keys/{id}/disable", s.auth(gateway.ScopeAdmin, s.handleDisableKey))
	mux.HandleFunc("DELETE /admin/keys/{id}", s.auth(gateway.ScopeAdmin, s.handleDeleteKey))

	// Access log endpoints.
	mux.HandleFunc("GET /admin/logs", s.auth(gateway.ScopeAdmin, s.handleListAccessLogs))
	mux.HandleFunc("GET /admin/keys/{id}/logs", s.auth(gateway.ScopeAdmin, s.handleKeyAccessLogs))
	return mux
}

type ctxKey int

const ctxKeyAuth ctxKey = 0

// responseWriter wraps http.ResponseWriter untuk menangkap status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// clientIP mengekstrak IP client dari request (perhatikan reverse proxy).
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		// Ambil IP pertama (client asli sebelum proxy)
		if idx := strings.Index(ip, ","); idx >= 0 {
			return strings.TrimSpace(ip[:idx])
		}
		return strings.TrimSpace(ip)
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	// Lepas port dari RemoteAddr
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx >= 0 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

// authKeyFrom returns the authenticated API key from the request context.
func authKeyFrom(r *http.Request) *gateway.APIKey {
	if v, ok := r.Context().Value(ctxKeyAuth).(*gateway.APIKey); ok {
		return v
	}
	return nil
}

// extractSecret reads the API key from X-API-Key or Authorization: Bearer.
func extractSecret(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return ""
}

// auth wraps a handler with API-key authentication, scope checking, and rate
// limiting. When no auth is configured (no master key and no managed keys),
// requests pass through as the master identity.
func (s *Server) auth(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := newResponseWriter(w)

		if !s.mgr.AuthRequired() {
			ctx := context.WithValue(r.Context(), ctxKeyAuth, s.mgr.Keys().MasterKey())
			next(rw, r.WithContext(ctx))
			s.mgr.AccessLog().Record(gateway.AccessLogEntry{
				KeyID:      "master",
				KeyName:    "master",
				Method:     r.Method,
				Path:       r.URL.Path,
				StatusCode: rw.statusCode,
				LatencyMs:  time.Since(start).Milliseconds(),
				IP:         clientIP(r),
			})
			return
		}

		secret := extractSecret(r)
		if secret == "" {
			writeError(rw, http.StatusUnauthorized, "missing API key (set X-API-Key header)")
			return
		}

		key, rr, err := s.mgr.Authenticate(secret)
		if err != nil {
			switch {
			case errors.Is(err, gateway.ErrKeyDisabled):
				writeError(rw, http.StatusForbidden, "api key is disabled")
			case errors.Is(err, gateway.ErrKeyExpired):
				writeError(rw, http.StatusForbidden, "api key has expired")
			default:
				writeError(rw, http.StatusUnauthorized, "invalid API key")
			}
			return
		}

		if rr.Limit > 0 {
			rw.Header().Set("X-RateLimit-Limit", strconv.Itoa(rr.Limit))
			rw.Header().Set("X-RateLimit-Remaining", strconv.Itoa(rr.Remaining))
			rw.Header().Set("X-RateLimit-Reset", strconv.FormatInt(rr.ResetAt, 10))
		}
		if !rr.Allowed {
			retry := rr.ResetAt - time.Now().Unix()
			if retry < 1 {
				retry = 1
			}
			rw.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
			writeError(rw, http.StatusTooManyRequests, "rate limit exceeded, try again later")
			s.mgr.AccessLog().Record(gateway.AccessLogEntry{
				KeyID:      key.ID,
				KeyName:    key.Name,
				Method:     r.Method,
				Path:       r.URL.Path,
				StatusCode: http.StatusTooManyRequests,
				LatencyMs:  time.Since(start).Milliseconds(),
				IP:         clientIP(r),
			})
			return
		}

		if !key.HasScope(scope) {
			writeError(rw, http.StatusForbidden, "insufficient scope: requires '"+scope+"'")
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyAuth, key)
		next(rw, r.WithContext(ctx))

		s.mgr.AccessLog().Record(gateway.AccessLogEntry{
			KeyID:      key.ID,
			KeyName:    key.Name,
			Method:     r.Method,
			Path:       r.URL.Path,
			StatusCode: rw.statusCode,
			LatencyMs:  time.Since(start).Milliseconds(),
			IP:         clientIP(r),
		})
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

	ownerKey := ""
	if key := authKeyFrom(r); key != nil && !key.Master {
		ownerKey = key.ID
		if key.MaxSessions > 0 {
			n, err := s.mgr.SessionCountByKey(ctx, key.ID)
			if err == nil && n >= key.MaxSessions {
				writeError(w, http.StatusForbidden, fmt.Sprintf("session limit reached (%d) for this API key", key.MaxSessions))
				return
			}
		}
	}

	sess, err := s.mgr.Create(ctx, req.Name, ownerKey)
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
		Order:   r.URL.Query().Get("order"),
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
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.After = n
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	msgs, err := s.mgr.Messages(ctx, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range msgs {
		if msgs[i].MediaKey != "" {
			msgs[i].MediaURL = "/messages/" + msgs[i].ID + "/media?session=" + url.QueryEscape(msgs[i].Session)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "count": len(msgs)})
}

// handleGetMedia streams a stored media file for a single message. The stored
// media key is derived server-side (never from user input) and resolved with a
// path-traversal guard by the media store.
func (s *Server) handleGetMedia(w http.ResponseWriter, r *http.Request) {
	if !s.mgr.StorageEnabled() {
		writeError(w, http.StatusNotImplemented, "message storage is disabled; set STORE_MESSAGES=true to enable")
		return
	}
	id := r.PathValue("id")
	session := r.URL.Query().Get("session")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	msg, ok, err := s.mgr.MediaByID(ctx, session, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if msg.MediaKey == "" {
		writeError(w, http.StatusNotFound, "message has no stored media")
		return
	}

	rc, size, err := s.mgr.OpenMedia(ctx, msg.MediaKey)
	if err != nil {
		writeError(w, http.StatusNotFound, "media file unavailable")
		return
	}
	defer rc.Close()

	ctype := msg.Mimetype
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if msg.Filename != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", msg.Filename))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
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

type normalizeRequest struct {
	Phones      []string `json:"phones"`
	CountryCode string   `json:"countryCode"`
}

type normalizeResult struct {
	Input      string `json:"input"`
	Normalized string `json:"normalized,omitempty"`
	Error      string `json:"error,omitempty"`
}

// handleNormalize menormalisasi daftar nomor telepon ke format internasional.
func (s *Server) handleNormalize(w http.ResponseWriter, r *http.Request) {
	var req normalizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Phones) == 0 {
		writeError(w, http.StatusBadRequest, "field 'phones' is required and must not be empty")
		return
	}

	cc := req.CountryCode
	if cc == "" {
		cc = s.cfg.DefaultCountryCode
	}

	results := make([]normalizeResult, 0, len(req.Phones))
	for _, p := range req.Phones {
		res := normalizeResult{Input: p}
		normalized, err := gateway.NormalizePhone(p, cc)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Normalized = normalized
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
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

type resolveLIDRequest struct {
	Session string   `json:"session"`
	LIDs    []string `json:"lids"`
}

// handleResolveLID memetakan JID @lid (alias privasi) ke JID nomor telepon asli
// memakai identity store session. LID yang tak dikenal tidak muncul di hasil.
func (s *Server) handleResolveLID(w http.ResponseWriter, r *http.Request) {
	var req resolveLIDRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.LIDs) == 0 {
		writeError(w, http.StatusBadRequest, "field 'lids' is required and must not be empty")
		return
	}
	if len(req.LIDs) > 500 {
		writeError(w, http.StatusBadRequest, "maximum 500 lids per request")
		return
	}

	sess, ok := s.session(req.Session, w)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	results := sess.ResolveLIDs(ctx, req.LIDs)
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results)})
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

// ---- API key management (admin) ----

type createKeyRequest struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	RateLimit     int      `json:"rateLimit"`
	RateWindowSec int      `json:"rateWindowSec"`
	MaxSessions   int      `json:"maxSessions"`
	ExpiresAt     int64    `json:"expiresAt"`
}

type updateKeyRequest struct {
	Name          *string   `json:"name"`
	Scopes        *[]string `json:"scopes"`
	RateLimit     *int      `json:"rateLimit"`
	RateWindowSec *int      `json:"rateWindowSec"`
	MaxSessions   *int      `json:"maxSessions"`
	Enabled       *bool     `json:"enabled"`
	ExpiresAt     *int64    `json:"expiresAt"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	key, err := s.mgr.Keys().Create(ctx, gateway.KeyCreateOptions{
		Name:          req.Name,
		Scopes:        req.Scopes,
		RateLimit:     req.RateLimit,
		RateWindowSec: req.RateWindowSec,
		MaxSessions:   req.MaxSessions,
		ExpiresAt:     req.ExpiresAt,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, key)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	keys, err := s.mgr.Keys().List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) handleGetKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	key, err := s.mgr.Keys().Get(ctx, r.PathValue("id"))
	if err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	var req updateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	key, err := s.mgr.Keys().Update(ctx, r.PathValue("id"), gateway.KeyUpdateOptions{
		Name:          req.Name,
		Scopes:        req.Scopes,
		RateLimit:     req.RateLimit,
		RateWindowSec: req.RateWindowSec,
		MaxSessions:   req.MaxSessions,
		Enabled:       req.Enabled,
		ExpiresAt:     req.ExpiresAt,
	})
	if err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	key, err := s.mgr.Keys().Rotate(ctx, r.PathValue("id"))
	if err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

func (s *Server) handleEnableKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	t := true
	key, err := s.mgr.Keys().Update(ctx, r.PathValue("id"), gateway.KeyUpdateOptions{Enabled: &t})
	if err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

func (s *Server) handleDisableKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	f := false
	key, err := s.mgr.Keys().Update(ctx, r.PathValue("id"), gateway.KeyUpdateOptions{Enabled: &f})
	if err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.mgr.Keys().Delete(ctx, r.PathValue("id")); err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) writeKeyError(w http.ResponseWriter, err error) {
	if errors.Is(err, gateway.ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
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

// handleListAccessLogs mengembalikan daftar access log (semua key).
// Query params: key (key_id), since (unix timestamp), limit (default 100, max 1000).
func (s *Server) handleListAccessLogs(w http.ResponseWriter, r *http.Request) {
	q := parseAccessLogQuery(r)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	entries, err := s.mgr.AccessLog().Query(ctx, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": entries, "count": len(entries)})
}

// handleKeyAccessLogs mengembalikan access log untuk satu key tertentu.
func (s *Server) handleKeyAccessLogs(w http.ResponseWriter, r *http.Request) {
	q := parseAccessLogQuery(r)
	q.KeyID = r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	entries, err := s.mgr.AccessLog().Query(ctx, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": entries, "count": len(entries)})
}

func parseAccessLogQuery(r *http.Request) gateway.AccessLogQuery {
	q := gateway.AccessLogQuery{
		KeyID: r.URL.Query().Get("key"),
		Limit: 100,
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.Limit = n
		}
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.Since = n
		}
	}
	return q
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
