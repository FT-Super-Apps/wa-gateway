package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Session represents a single WhatsApp connection (one phone number).
type Session struct {
	mgr  *Manager
	name string
	wa   *whatsmeow.Client
	log  waLog.Logger

	mu        sync.RWMutex
	latestQR  string
	pairError string
}

func newSession(mgr *Manager, name string, wa *whatsmeow.Client) *Session {
	s := &Session{
		mgr:  mgr,
		name: name,
		wa:   wa,
		log:  waLog.Stdout("Session/"+name, mgr.cfg.LogLevel, true),
	}
	wa.AddEventHandler(s.handleEvent)
	return s
}

// start connects to WhatsApp, beginning QR pairing if the device is new.
func (s *Session) start(ctx context.Context) error {
	if s.wa.Store.ID == nil {
		qrChan, err := s.wa.GetQRChannel(context.Background())
		if err != nil {
			return fmt.Errorf("get qr channel: %w", err)
		}
		if err := s.wa.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		go s.consumeQR(qrChan)
		return nil
	}
	return s.wa.Connect()
}

func (s *Session) consumeQR(qrChan <-chan whatsmeow.QRChannelItem) {
	for item := range qrChan {
		switch item.Event {
		case "code":
			s.mu.Lock()
			s.latestQR = item.Code
			s.pairError = ""
			s.mu.Unlock()
			s.log.Infof("New QR code, scan via GET /qr?session=%s", s.name)
		case "success":
			s.mu.Lock()
			s.latestQR = ""
			s.pairError = ""
			s.mu.Unlock()
			s.log.Infof("Pairing successful")
		default:
			if item.Error != nil {
				s.mu.Lock()
				s.latestQR = ""
				s.pairError = item.Error.Error()
				s.mu.Unlock()
				s.log.Errorf("Pairing error: %v", item.Error)
			}
		}
	}
}

func (s *Session) stop() {
	s.wa.Disconnect()
}

// Status describes the current connection and login state of a session.
type Status struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"loggedIn"`
	JID       string `json:"jid,omitempty"`
	PushName  string `json:"pushName,omitempty"`
	HasQR     bool   `json:"hasQR"`
	PairError string `json:"pairError,omitempty"`
}

// IsReady reports whether the session is connected and logged in, i.e. able to
// send messages immediately.
func (s *Session) IsReady() bool {
	return s.wa.IsConnected() && s.wa.IsLoggedIn()
}

// Status returns the current session status.
func (s *Session) Status() Status {
	s.mu.RLock()
	qr := s.latestQR
	pairErr := s.pairError
	s.mu.RUnlock()

	st := Status{
		Name:      s.name,
		Connected: s.wa.IsConnected(),
		LoggedIn:  s.wa.IsLoggedIn(),
		HasQR:     qr != "",
		PairError: pairErr,
	}
	if id := s.wa.Store.ID; id != nil {
		st.JID = id.String()
		st.PushName = s.wa.Store.PushName
	}
	return st
}

// CurrentQR returns the latest pending QR code, or empty if none.
func (s *Session) CurrentQR() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latestQR
}

// PairPhone requests a pairing code for the given phone number as an
// alternative to QR scanning. The returned 8-character code is entered on the
// phone via WhatsApp > Linked Devices > Link with phone number instead.
func (s *Session) PairPhone(ctx context.Context, phone string) (string, error) {
	if s.wa.IsLoggedIn() {
		return "", errors.New("session is already logged in")
	}

	jid, err := parseJID(phone, s.mgr.cfg.DefaultCountryCode)
	if err != nil {
		return "", err
	}
	number := jid.User

	if !s.wa.IsConnected() {
		if err := s.wa.Connect(); err != nil {
			return "", fmt.Errorf("connect: %w", err)
		}
	}

	code, err := s.wa.PairPhone(ctx, number, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		return "", fmt.Errorf("pair phone: %w", err)
	}
	s.log.Infof("Pairing code for +%s: %s", number, code)
	return code, nil
}

// PhoneCheckResult adalah hasil cek satu nomor.
type PhoneCheckResult struct {
	Phone        string `json:"phone"`
	JID          string `json:"jid"`
	IsOnWhatsApp bool   `json:"isOnWhatsApp"`
	IsBusiness   bool   `json:"isBusiness"`
	BusinessName string `json:"businessName,omitempty"`
}

// CheckPhones memeriksa apakah nomor-nomor yang diberikan terdaftar di WhatsApp.
// Maksimal 250 nomor per panggilan (batasan WhatsApp).
func (s *Session) CheckPhones(ctx context.Context, phones []string) ([]PhoneCheckResult, error) {
	if !s.wa.IsLoggedIn() {
		return nil, errors.New("session is not logged in")
	}

	normalized := make([]string, 0, len(phones))
	phoneMap := make(map[string]string, len(phones)) // normalizedUser → input asli
	for _, p := range phones {
		jid, err := parseJID(p, s.mgr.cfg.DefaultCountryCode)
		if err != nil {
			return nil, fmt.Errorf("invalid phone %q: %w", p, err)
		}
		normalized = append(normalized, "+"+jid.User)
		phoneMap[jid.User] = p
	}

	resp, err := s.wa.IsOnWhatsApp(ctx, normalized)
	if err != nil {
		return nil, fmt.Errorf("check phones: %w", err)
	}

	out := make([]PhoneCheckResult, 0, len(resp))
	for _, r := range resp {
		res := PhoneCheckResult{
			Phone:        phoneMap[r.JID.User],
			JID:          r.JID.String(),
			IsOnWhatsApp: r.IsIn,
		}
		if r.VerifiedName != nil {
			res.IsBusiness = true
			res.BusinessName = r.VerifiedName.Details.GetVerifiedName()
		}
		out = append(out, res)
	}
	return out, nil
}

// Logout logs the session out and removes its stored credentials.
func (s *Session) Logout(ctx context.Context) error {
	return s.wa.Logout(ctx)
}

// GroupInfo is a lightweight description of a joined WhatsApp group.
type GroupInfo struct {
	JID          string `json:"jid"`
	Name         string `json:"name"`
	Participants int    `json:"participants"`
	IsAnnounce   bool   `json:"isAnnounce"`
}

// ListGroups returns the groups the session's account has joined.
func (s *Session) ListGroups(ctx context.Context) ([]GroupInfo, error) {
	if !s.wa.IsLoggedIn() {
		return nil, errors.New("session is not logged in")
	}
	groups, err := s.wa.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GroupInfo, 0, len(groups))
	for _, g := range groups {
		out = append(out, GroupInfo{
			JID:          g.JID.String(),
			Name:         g.GroupName.Name,
			Participants: len(g.Participants),
			IsAnnounce:   g.GroupAnnounce.IsAnnounce,
		})
	}
	return out, nil
}

// SendText sends a plain text message.
func (s *Session) SendText(ctx context.Context, to, text string) (string, error) {
	jid, err := parseJID(to, s.mgr.cfg.DefaultCountryCode)
	if err != nil {
		return "", err
	}
	msg := &waProto.Message{Conversation: proto.String(text)}
	resp, err := s.wa.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", err
	}
	s.recordOutgoing(jid, resp.ID, "text", text, nil)
	return resp.ID, nil
}

// MediaInput describes a media payload to send.
type MediaInput struct {
	Data     []byte
	Mimetype string
	Filename string
	Caption  string
	Seconds  uint32
}

// SendImage uploads and sends an image message.
func (s *Session) SendImage(ctx context.Context, to string, in MediaInput) (string, error) {
	jid, err := parseJID(to, s.mgr.cfg.DefaultCountryCode)
	if err != nil {
		return "", err
	}
	up, err := s.wa.Upload(ctx, in.Data, whatsmeow.MediaImage)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	mimetype := in.Mimetype
	if mimetype == "" {
		mimetype = "image/jpeg"
	}
	msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{
		Caption:       proto.String(in.Caption),
		Mimetype:      proto.String(mimetype),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}
	resp, err := s.wa.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", err
	}
	s.recordOutgoing(jid, resp.ID, "image", in.Caption, &in)
	return resp.ID, nil
}

// SendFile uploads and sends a document message.
func (s *Session) SendFile(ctx context.Context, to string, in MediaInput) (string, error) {
	jid, err := parseJID(to, s.mgr.cfg.DefaultCountryCode)
	if err != nil {
		return "", err
	}
	up, err := s.wa.Upload(ctx, in.Data, whatsmeow.MediaDocument)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	mimetype := in.Mimetype
	if mimetype == "" {
		mimetype = "application/octet-stream"
	}
	filename := in.Filename
	if filename == "" {
		filename = "file"
	}
	msg := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
		Caption:       proto.String(in.Caption),
		Mimetype:      proto.String(mimetype),
		FileName:      proto.String(filename),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}
	resp, err := s.wa.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", err
	}
	s.recordOutgoing(jid, resp.ID, "document", in.Caption, &in)
	return resp.ID, nil
}

// SendVoice uploads and sends a voice note (push-to-talk audio).
func (s *Session) SendVoice(ctx context.Context, to string, in MediaInput) (string, error) {
	jid, err := parseJID(to, s.mgr.cfg.DefaultCountryCode)
	if err != nil {
		return "", err
	}
	up, err := s.wa.Upload(ctx, in.Data, whatsmeow.MediaAudio)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	mimetype := in.Mimetype
	if mimetype == "" {
		mimetype = "audio/ogg; codecs=opus"
	}
	msg := &waProto.Message{AudioMessage: &waProto.AudioMessage{
		PTT:           proto.Bool(true),
		Mimetype:      proto.String(mimetype),
		Seconds:       proto.Uint32(in.Seconds),
		URL:           proto.String(up.URL),
		DirectPath:    proto.String(up.DirectPath),
		MediaKey:      up.MediaKey,
		FileEncSHA256: up.FileEncSHA256,
		FileSHA256:    up.FileSHA256,
		FileLength:    proto.Uint64(up.FileLength),
	}}
	resp, err := s.wa.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", err
	}
	s.recordOutgoing(jid, resp.ID, "audio", "", &in)
	return resp.ID, nil
}

// handleEvent dispatches whatsmeow events for this session.
func (s *Session) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		s.mgr.notifier.enqueue(s.name, s.wa, v)
		s.recordIncoming(v)
	case *events.PairSuccess:
		s.mgr.bindJID(s.name, v.ID)
		s.log.Infof("Paired as %s", v.ID)
	case *events.Connected:
		s.log.Infof("Connected to WhatsApp")
	case *events.LoggedOut:
		s.log.Warnf("Logged out: %v", v.Reason)
	}
}

// recordIncoming persists an incoming (or self-echo) message when storage is
// enabled and the chat passes the store filter.
func (s *Session) recordIncoming(v *events.Message) {
	chat := v.Info.Chat.String()
	if !s.mgr.filter.allowChat(chat) {
		return
	}
	body, typ := extractText(v.Message)
	direction := "in"
	if v.Info.IsFromMe {
		direction = "out"
	}
	rec := StoredMessage{
		ID:        v.Info.ID,
		Session:   s.name,
		Chat:      chat,
		Sender:    v.Info.Sender.String(),
		Direction: direction,
		FromMe:    v.Info.IsFromMe,
		IsGroup:   v.Info.IsGroup,
		Type:      typ,
		Body:      body,
		Timestamp: v.Info.Timestamp.Unix(),
	}
	s.mgr.store.save(rec)

	if s.mgr.cfg.StoreMedia {
		if dl, mm, ok := mediaInfo(v.Message); ok {
			go s.persistIncomingMedia(rec.Session, rec.ID, dl, mm)
		}
	}
}

// persistIncomingMedia downloads an incoming media attachment and records its
// storage key. It runs in its own goroutine so it never blocks the whatsmeow
// event loop.
func (s *Session) persistIncomingMedia(session, id string, dl whatsmeow.DownloadableMessage, mm mediaMeta) {
	if s.mgr.cfg.MaxDownloadByte > 0 && int64(mm.FileLength) > s.mgr.cfg.MaxDownloadByte {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	data, err := s.wa.Download(ctx, dl)
	if err != nil {
		s.log.Warnf("download media for %s: %v", id, err)
		return
	}
	key, err := s.mgr.media.Put(ctx, session, id, extFromMime(mm.Mimetype), data)
	if err != nil {
		s.log.Errorf("store media for %s: %v", id, err)
		return
	}
	s.mgr.store.updateMedia(session, id, key, mm.Mimetype, mm.Filename, int64(len(data)))
}

// recordOutgoing persists a message sent via the API when storage is enabled and
// the chat passes the store filter. When media is provided, its bytes are stored
// too (no re-download needed since we already hold them).
func (s *Session) recordOutgoing(jid types.JID, id, msgType, body string, media *MediaInput) {
	chat := jid.String()
	if !s.mgr.filter.allowChat(chat) {
		return
	}
	var sender string
	if s.wa.Store != nil && s.wa.Store.ID != nil {
		sender = s.wa.Store.ID.String()
	}
	rec := StoredMessage{
		ID:        id,
		Session:   s.name,
		Chat:      chat,
		Sender:    sender,
		Direction: "out",
		FromMe:    true,
		IsGroup:   jid.Server == types.GroupServer,
		Type:      msgType,
		Body:      body,
		Timestamp: time.Now().Unix(),
	}
	if s.mgr.cfg.StoreMedia && media != nil && len(media.Data) > 0 {
		key, err := s.mgr.media.Put(context.Background(), rec.Session, rec.ID, extFromMime(media.Mimetype), media.Data)
		if err != nil {
			s.log.Errorf("store outgoing media for %s: %v", id, err)
		} else {
			rec.MediaKey = key
			rec.Mimetype = media.Mimetype
			rec.Filename = media.Filename
			rec.FileLength = int64(len(media.Data))
		}
	}
	s.mgr.store.save(rec)
}

// NormalizePhone menormalisasi berbagai format input nomor telepon ke format
// internasional hanya angka (contoh: "628114100444").
//
// Yang di-strip: spasi, tanda hubung, titik, kurung, tanda sama dengan, slash,
// dan karakter non-digit lainnya. Tanda "+" di awal dikenali sebagai penanda
// format internasional (tidak memerlukan DEFAULT_COUNTRY_CODE).
//
// Contoh:
//
//	"0812345678"      + cc="62"  → "62812345678"
//	"0812=345-678"   + cc="62"  → "62812345678"
//	"+6281-234-5678" + cc=""    → "62812345678"
//	"(628) 114.100444"           → "628114100444"
func NormalizePhone(raw, defaultCC string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("phone number is empty")
	}

	hasPlus := strings.HasPrefix(raw, "+")

	// Strip semua karakter non-digit
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	raw = b.String()

	if raw == "" {
		return "", errors.New("phone number contains no digits")
	}

	// Nomor lokal: ganti leading "0" dengan country code
	if !hasPlus && defaultCC != "" && strings.HasPrefix(raw, "0") {
		raw = defaultCC + raw[1:]
	}

	if strings.HasPrefix(raw, "0") {
		return "", fmt.Errorf("phone number must be in international format without leading 0 (got %q); set DEFAULT_COUNTRY_CODE to auto-convert", raw)
	}

	if len(raw) < 7 {
		return "", fmt.Errorf("phone number too short: %q", raw)
	}

	return raw, nil
}

// parseJID normalizes various phone/JID formats into a WhatsApp JID. When
// defaultCC is set, a local number with a leading "0" is converted to
// international format (e.g. "08114100444" -> "628114100444" for cc "62").
func parseJID(raw, defaultCC string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, errors.New("recipient is empty")
	}

	if strings.HasSuffix(raw, "@g.us") || strings.HasSuffix(raw, "@s.whatsapp.net") || strings.HasSuffix(raw, "@newsletter") {
		jid, err := types.ParseJID(raw)
		if err != nil {
			return types.JID{}, fmt.Errorf("invalid jid: %w", err)
		}
		return jid, nil
	}

	if strings.HasSuffix(raw, "@c.us") {
		raw = strings.TrimSuffix(raw, "@c.us")
	}

	number, err := NormalizePhone(raw, defaultCC)
	if err != nil {
		return types.JID{}, err
	}
	return types.NewJID(number, types.DefaultUserServer), nil
}
