package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"wa-gateway/internal/config"
)

// webhookNotifier forwards incoming events to the configured webhook URL using a
// bounded worker queue with retry/backoff.
type webhookNotifier struct {
	cfg    *config.Config
	client *http.Client
	log    waLog.Logger

	queue chan []byte
	wg    sync.WaitGroup
	quit  chan struct{}
	once  sync.Once
}

func newWebhookNotifier(cfg *config.Config) *webhookNotifier {
	size := cfg.WebhookQueueSize
	if size < 1 {
		size = 1
	}
	return &webhookNotifier{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		log:    waLog.Stdout("Webhook", cfg.LogLevel, true),
		queue:  make(chan []byte, size),
		quit:   make(chan struct{}),
	}
}

// start launches the worker pool.
func (n *webhookNotifier) start() {
	if n.cfg.WebhookURL == "" {
		return
	}
	workers := n.cfg.WebhookWorkers
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		n.wg.Add(1)
		go n.worker()
	}
}

// stop drains and shuts down the worker pool.
func (n *webhookNotifier) stop() {
	if n.cfg.WebhookURL == "" {
		return
	}
	n.once.Do(func() { close(n.quit) })
	n.wg.Wait()
}

func (n *webhookNotifier) worker() {
	defer n.wg.Done()
	for {
		select {
		case <-n.quit:
			return
		case body := <-n.queue:
			n.deliver(body)
		}
	}
}

type mediaPayload struct {
	Mimetype   string `json:"mimetype,omitempty"`
	Filename   string `json:"filename,omitempty"`
	FileLength uint64 `json:"fileLength,omitempty"`
	DataBase64 string `json:"dataBase64,omitempty"`
	Error      string `json:"error,omitempty"`
}

type messagePayload struct {
	ID        string        `json:"id"`
	Timestamp int64         `json:"timestamp"`
	From      string        `json:"from"`
	Sender    string        `json:"sender"`
	PushName  string        `json:"pushName,omitempty"`
	FromMe    bool          `json:"fromMe"`
	IsGroup   bool          `json:"isGroup"`
	Type      string        `json:"type"`
	Body      string        `json:"body,omitempty"`
	HasMedia  bool          `json:"hasMedia"`
	Media     *mediaPayload `json:"media,omitempty"`
}

type webhookEnvelope struct {
	Event   string `json:"event"`
	Session string `json:"session"`
	Payload any    `json:"payload"`
}

// receiptPayload carries a delivery/read receipt (WhatsApp check marks) for one
// or more previously sent messages.
type receiptPayload struct {
	Chat       string   `json:"chat"`
	Sender     string   `json:"sender"`
	MessageIDs []string `json:"messageIds"`
	Status     string   `json:"status"` // delivered|read|played|read-self|played-self
	Timestamp  int64    `json:"timestamp"`
	IsGroup    bool     `json:"isGroup"`
	FromMe     bool     `json:"fromMe"`
}

// receiptStatus maps a whatsmeow receipt type to a friendly status label. The
// second result is false for receipt types that should not be forwarded
// (retry, sender, server-error, inactive, peer messages, history sync).
func receiptStatus(t types.ReceiptType) (string, bool) {
	switch t {
	case types.ReceiptTypeDelivered:
		return "delivered", true
	case types.ReceiptTypeRead:
		return "read", true
	case types.ReceiptTypePlayed:
		return "played", true
	case types.ReceiptTypeReadSelf:
		return "read-self", true
	case types.ReceiptTypePlayedSelf:
		return "played-self", true
	default:
		return "", false
	}
}

// enqueue builds the payload and pushes it onto the queue (non-blocking).
func (n *webhookNotifier) enqueue(session string, wa *whatsmeow.Client, evt *events.Message) {
	if n.cfg.WebhookURL == "" || !n.cfg.WantsEvent("message") {
		return
	}

	p := messagePayload{
		ID:        evt.Info.ID,
		Timestamp: evt.Info.Timestamp.Unix(),
		From:      evt.Info.Chat.String(),
		Sender:    evt.Info.Sender.String(),
		PushName:  evt.Info.PushName,
		FromMe:    evt.Info.IsFromMe,
		IsGroup:   evt.Info.IsGroup,
	}
	p.Body, p.Type = extractText(evt.Message)
	n.attachMedia(context.Background(), wa, evt.Message, &p)

	body, err := json.Marshal(webhookEnvelope{Event: "message", Session: session, Payload: p})
	if err != nil {
		n.log.Errorf("marshal webhook payload: %v", err)
		return
	}

	select {
	case n.queue <- body:
	default:
		n.log.Warnf("webhook queue full, dropping message %s", evt.Info.ID)
	}
}

// enqueueReceipt builds a receipt payload and pushes it onto the queue
// (non-blocking). Emitted only when the "receipt" event is enabled.
func (n *webhookNotifier) enqueueReceipt(session string, evt *events.Receipt) {
	if n.cfg.WebhookURL == "" || !n.cfg.WantsEvent("receipt") {
		return
	}
	status, ok := receiptStatus(evt.Type)
	if !ok || len(evt.MessageIDs) == 0 {
		return
	}
	ids := make([]string, len(evt.MessageIDs))
	for i, id := range evt.MessageIDs {
		ids[i] = string(id)
	}
	p := receiptPayload{
		Chat:       evt.Chat.String(),
		Sender:     evt.Sender.String(),
		MessageIDs: ids,
		Status:     status,
		Timestamp:  evt.Timestamp.Unix(),
		IsGroup:    evt.IsGroup,
		FromMe:     evt.IsFromMe,
	}
	body, err := json.Marshal(webhookEnvelope{Event: "receipt", Session: session, Payload: p})
	if err != nil {
		n.log.Errorf("marshal receipt payload: %v", err)
		return
	}

	select {
	case n.queue <- body:
	default:
		n.log.Warnf("webhook queue full, dropping receipt for %v", ids)
	}
}

// deliver attempts delivery with exponential backoff up to the configured retries.
func (n *webhookNotifier) deliver(body []byte) {
	attempts := n.cfg.WebhookMaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	backoff := time.Duration(n.cfg.WebhookBackoffMS) * time.Millisecond
	if backoff <= 0 {
		backoff = time.Second
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		if n.post(body) {
			return
		}
		if attempt < attempts {
			wait := backoff * time.Duration(1<<(attempt-1))
			n.log.Warnf("webhook delivery failed (attempt %d/%d), retrying in %s", attempt, attempts, wait)
			select {
			case <-time.After(wait):
			case <-n.quit:
				return
			}
		} else {
			n.log.Errorf("webhook delivery failed after %d attempts, giving up", attempts)
		}
	}
}

// post performs a single delivery attempt; returns true on success (2xx).
func (n *webhookNotifier) post(body []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		n.log.Errorf("build webhook request: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if n.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", n.cfg.APIKey)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		n.log.Errorf("post webhook: %v", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 300
}

func extractText(msg *waProto.Message) (body, msgType string) {
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation(), "text"
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText(), "text"
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetCaption(), "image"
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetCaption(), "video"
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetCaption(), "document"
	case msg.GetAudioMessage() != nil:
		return "", "audio"
	case msg.GetStickerMessage() != nil:
		return "", "sticker"
	default:
		return "", "unknown"
	}
}

// mediaMeta describes a downloadable media attachment.
type mediaMeta struct {
	Mimetype   string
	Filename   string
	FileLength uint64
}

// mediaInfo extracts the downloadable handle and metadata for any media message,
// or ok=false when the message carries no media. Shared by the webhook payload
// builder and the message store's media persistence.
func mediaInfo(msg *waProto.Message) (whatsmeow.DownloadableMessage, mediaMeta, bool) {
	switch {
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		return m, mediaMeta{Mimetype: m.GetMimetype(), FileLength: m.GetFileLength()}, true
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		return m, mediaMeta{Mimetype: m.GetMimetype(), FileLength: m.GetFileLength()}, true
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		return m, mediaMeta{Mimetype: m.GetMimetype(), FileLength: m.GetFileLength()}, true
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		return m, mediaMeta{Mimetype: m.GetMimetype(), Filename: m.GetFileName(), FileLength: m.GetFileLength()}, true
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		return m, mediaMeta{Mimetype: m.GetMimetype(), FileLength: m.GetFileLength()}, true
	default:
		return nil, mediaMeta{}, false
	}
}

func (n *webhookNotifier) attachMedia(ctx context.Context, wa *whatsmeow.Client, msg *waProto.Message, p *messagePayload) {
	downloadable, mm, ok := mediaInfo(msg)
	if !ok {
		return
	}
	media := mediaPayload{Mimetype: mm.Mimetype, Filename: mm.Filename, FileLength: mm.FileLength}
	p.HasMedia = true

	if !n.cfg.DownloadMedia {
		p.Media = &media
		return
	}
	if n.cfg.MaxDownloadByte > 0 && int64(media.FileLength) > n.cfg.MaxDownloadByte {
		media.Error = "file exceeds MAX_DOWNLOAD_BYTES, not downloaded"
		p.Media = &media
		return
	}

	data, err := wa.Download(ctx, downloadable)
	if err != nil {
		media.Error = err.Error()
		p.Media = &media
		return
	}
	media.DataBase64 = base64.StdEncoding.EncodeToString(data)
	p.Media = &media
}
