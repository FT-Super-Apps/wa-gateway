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
	s.recordOutgoing(jid, resp.ID, "text", text)
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
	s.recordOutgoing(jid, resp.ID, "image", in.Caption)
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
	s.recordOutgoing(jid, resp.ID, "document", in.Caption)
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
	s.recordOutgoing(jid, resp.ID, "audio", "")
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

// recordIncoming persists an incoming (or self-echo) message when storage is enabled.
func (s *Session) recordIncoming(v *events.Message) {
	body, typ := extractText(v.Message)
	direction := "in"
	if v.Info.IsFromMe {
		direction = "out"
	}
	s.mgr.store.save(StoredMessage{
		ID:        v.Info.ID,
		Session:   s.name,
		Chat:      v.Info.Chat.String(),
		Sender:    v.Info.Sender.String(),
		Direction: direction,
		FromMe:    v.Info.IsFromMe,
		IsGroup:   v.Info.IsGroup,
		Type:      typ,
		Body:      body,
		Timestamp: v.Info.Timestamp.Unix(),
	})
}

// recordOutgoing persists a message sent via the API when storage is enabled.
func (s *Session) recordOutgoing(jid types.JID, id, msgType, body string) {
	var sender string
	if s.wa.Store != nil && s.wa.Store.ID != nil {
		sender = s.wa.Store.ID.String()
	}
	s.mgr.store.save(StoredMessage{
		ID:        id,
		Session:   s.name,
		Chat:      jid.String(),
		Sender:    sender,
		Direction: "out",
		FromMe:    true,
		IsGroup:   jid.Server == types.GroupServer,
		Type:      msgType,
		Body:      body,
		Timestamp: time.Now().Unix(),
	})
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

	raw = strings.TrimPrefix(raw, "+")

	if defaultCC != "" && strings.HasPrefix(raw, "0") {
		raw = defaultCC + strings.TrimPrefix(raw, "0")
	}

	for _, r := range raw {
		if r < '0' || r > '9' {
			return types.JID{}, fmt.Errorf("invalid phone number: %q", raw)
		}
	}
	if strings.HasPrefix(raw, "0") {
		return types.JID{}, fmt.Errorf("phone number must be in international format without leading 0 (got %q); set DEFAULT_COUNTRY_CODE to auto-convert", raw)
	}
	return types.NewJID(raw, types.DefaultUserServer), nil
}
