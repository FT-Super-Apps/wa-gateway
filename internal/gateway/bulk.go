package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// tmplVarRe matches placeholders like {{name}} or {{ nilai }}.
var tmplVarRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// renderTemplate replaces {{key}} placeholders with values from vars. Unknown
// placeholders are left untouched.
func renderTemplate(tmpl string, vars map[string]string) string {
	if tmpl == "" || len(vars) == 0 {
		return tmpl
	}
	return tmplVarRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := tmplVarRe.FindStringSubmatch(m)[1]
		if v, ok := vars[key]; ok {
			return v
		}
		return m
	})
}

// BulkMessage is a single recipient within a bulk job. Text (if set) is sent
// verbatim; otherwise the job template is rendered with Vars.
type BulkMessage struct {
	To   string            `json:"to"`
	Text string            `json:"text"`
	Vars map[string]string `json:"vars"`
}

// BulkRequest describes a mass-send job.
type BulkRequest struct {
	Session    string        `json:"session"`
	To         []string      `json:"to"`       // recipients sharing the same Text/Template
	Text       string        `json:"text"`     // plain message used with To
	Template   string        `json:"template"` // template with {{vars}}, rendered per recipient
	Messages   []BulkMessage `json:"messages"` // per-recipient text/vars; takes precedence over To
	MinDelayMS int           `json:"minDelayMs"`
	MaxDelayMS int           `json:"maxDelayMs"`
}

// BulkResult is the outcome of sending to a single recipient.
type BulkResult struct {
	To        string `json:"to"`
	Status    string `json:"status"` // "sent" or "failed"
	MessageID string `json:"messageId,omitempty"`
	Error     string `json:"error,omitempty"`
}

// BulkJob tracks the progress of a bulk send.
type BulkJob struct {
	ID         string       `json:"id"`
	Session    string       `json:"session"`
	Status     string       `json:"status"` // "running", "completed", "cancelled"
	Total      int          `json:"total"`
	Sent       int          `json:"sent"`
	Failed     int          `json:"failed"`
	StartedAt  int64        `json:"startedAt"`
	FinishedAt int64        `json:"finishedAt,omitempty"`
	Results    []BulkResult `json:"results"`
}

// snapshot returns a deep copy safe to serialize without holding the lock.
func (j *BulkJob) snapshot() BulkJob {
	cp := *j
	cp.Results = append([]BulkResult(nil), j.Results...)
	return cp
}

// bulkRunner executes bulk jobs sequentially per job with delay + jitter.
type bulkRunner struct {
	mgr *Manager
	log waLog.Logger

	mu   sync.Mutex
	jobs map[string]*BulkJob

	quit chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func newBulkRunner(mgr *Manager) *bulkRunner {
	return &bulkRunner{
		mgr:  mgr,
		log:  waLog.Stdout("Bulk", mgr.cfg.LogLevel, true),
		jobs: make(map[string]*BulkJob),
		quit: make(chan struct{}),
	}
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// submit validates the request, registers a job, and starts processing it.
func (b *bulkRunner) submit(req BulkRequest) (BulkJob, error) {
	session := req.Session
	if session == "" {
		session = "default"
	}
	if _, err := b.mgr.Get(session); err != nil {
		return BulkJob{}, err
	}

	msgs := req.Messages
	if len(msgs) == 0 {
		for _, to := range req.To {
			msgs = append(msgs, BulkMessage{To: to})
		}
	}
	if len(msgs) == 0 {
		return BulkJob{}, fmt.Errorf("no recipients: provide 'to' or 'messages'")
	}

	// Resolve the final text for each recipient.
	// Precedence: message.text > rendered(template, vars) > request.text.
	for i := range msgs {
		if msgs[i].To == "" {
			return BulkJob{}, fmt.Errorf("recipient #%d has empty 'to'", i+1)
		}
		text := msgs[i].Text
		if text == "" && req.Template != "" {
			text = renderTemplate(req.Template, msgs[i].Vars)
		}
		if text == "" {
			text = req.Text
		}
		if text == "" {
			return BulkJob{}, fmt.Errorf("recipient #%d (%s) has no message: provide 'text', a 'template' with 'vars', or top-level 'text'", i+1, msgs[i].To)
		}
		msgs[i].Text = text
	}

	minD := req.MinDelayMS
	if minD < 0 {
		minD = 0
	}
	if req.MinDelayMS == 0 {
		minD = b.mgr.cfg.BulkMinDelayMS
	}
	maxD := req.MaxDelayMS
	if req.MaxDelayMS == 0 {
		maxD = b.mgr.cfg.BulkMaxDelayMS
	}
	if maxD < minD {
		maxD = minD
	}

	job := &BulkJob{
		ID:        newJobID(),
		Session:   session,
		Status:    "running",
		Total:     len(msgs),
		StartedAt: time.Now().Unix(),
		Results:   make([]BulkResult, 0, len(msgs)),
	}

	b.mu.Lock()
	b.cleanupLocked()
	b.jobs[job.ID] = job
	b.mu.Unlock()

	b.wg.Add(1)
	go b.run(job, session, msgs, minD, maxD)

	return job.snapshot(), nil
}

// run sends each message sequentially, sleeping a jittered delay between sends.
func (b *bulkRunner) run(job *BulkJob, session string, msgs []BulkMessage, minD, maxD int) {
	defer b.wg.Done()

	for i, m := range msgs {
		select {
		case <-b.quit:
			b.finish(job, "cancelled")
			return
		default:
		}

		res := BulkResult{To: m.To, Status: "sent"}
		sess, err := b.mgr.Get(session)
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			id, sErr := sess.SendText(ctx, m.To, m.Text)
			cancel()
			if sErr != nil {
				res.Status, res.Error = "failed", sErr.Error()
			} else {
				res.MessageID = id
			}
		}

		b.mu.Lock()
		job.Results = append(job.Results, res)
		if res.Status == "sent" {
			job.Sent++
		} else {
			job.Failed++
		}
		b.mu.Unlock()

		if i < len(msgs)-1 {
			if !b.sleepJitter(minD, maxD) {
				b.finish(job, "cancelled")
				return
			}
		}
	}
	b.finish(job, "completed")
}

// sleepJitter waits a random duration in [minD, maxD] ms; returns false if cancelled.
func (b *bulkRunner) sleepJitter(minD, maxD int) bool {
	d := minD
	if maxD > minD {
		span := maxD - minD
		buf := make([]byte, 2)
		_, _ = rand.Read(buf)
		d = minD + int((uint16(buf[0])<<8|uint16(buf[1])))%(span+1)
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(time.Duration(d) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-b.quit:
		return false
	case <-timer.C:
		return true
	}
}

func (b *bulkRunner) finish(job *BulkJob, status string) {
	b.mu.Lock()
	if job.Status == "running" {
		job.Status = status
		job.FinishedAt = time.Now().Unix()
	}
	b.mu.Unlock()
}

// job returns a snapshot of a job by ID.
func (b *bulkRunner) job(id string) (BulkJob, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return BulkJob{}, false
	}
	return j.snapshot(), true
}

// list returns snapshots of all jobs, newest first.
func (b *bulkRunner) list() []BulkJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BulkJob, 0, len(b.jobs))
	for _, j := range b.jobs {
		out = append(out, j.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// cleanupLocked removes finished jobs older than one hour. Caller holds b.mu.
func (b *bulkRunner) cleanupLocked() {
	cutoff := time.Now().Add(-time.Hour).Unix()
	for id, j := range b.jobs {
		if j.Status != "running" && j.FinishedAt > 0 && j.FinishedAt < cutoff {
			delete(b.jobs, id)
		}
	}
}

func (b *bulkRunner) stop() {
	b.once.Do(func() { close(b.quit) })
	b.wg.Wait()
}
