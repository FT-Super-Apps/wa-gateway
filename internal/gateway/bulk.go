package gateway

import (
	"context"
	"crypto/rand"
	"database/sql"
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
	Status    string `json:"status"` // "pending", "sent", "failed"
	MessageID string `json:"messageId,omitempty"`
	Error     string `json:"error,omitempty"`
}

// BulkJob tracks the progress of a bulk send.
// Status: "running" | "completed" | "cancelled" | "interrupted"
// "interrupted" means the service crashed/restarted while the job was running.
// Pending recipients (status="pending" in Results) were NOT sent.
type BulkJob struct {
	ID         string       `json:"id"`
	Session    string       `json:"session"`
	Status     string       `json:"status"`
	Total      int          `json:"total"`
	Sent       int          `json:"sent"`
	Failed     int          `json:"failed"`
	StartedAt  int64        `json:"startedAt"`
	FinishedAt int64        `json:"finishedAt,omitempty"`
	Results    []BulkResult `json:"results"`
}

// Pending returns results with status "pending" (not yet attempted).
func (j *BulkJob) Pending() []BulkResult {
	var out []BulkResult
	for _, r := range j.Results {
		if r.Status == "pending" {
			out = append(out, r)
		}
	}
	return out
}

// snapshot returns a deep copy safe to serialize without holding the lock.
func (j *BulkJob) snapshot() BulkJob {
	cp := *j
	cp.Results = append([]BulkResult(nil), j.Results...)
	return cp
}

// ---- persistence -----------------------------------------------------------

// bulkStore handles SQLite persistence for bulk jobs.
type bulkStore struct {
	db  *sql.DB
	log waLog.Logger
}

func newBulkStore(db *sql.DB, log waLog.Logger) *bulkStore {
	return &bulkStore{db: db, log: log}
}

func (s *bulkStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS gw_bulk_jobs (
			id           TEXT PRIMARY KEY,
			session      TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL DEFAULT 'running',
			total        INTEGER NOT NULL DEFAULT 0,
			sent         INTEGER NOT NULL DEFAULT 0,
			failed       INTEGER NOT NULL DEFAULT 0,
			started_at   INTEGER NOT NULL DEFAULT 0,
			finished_at  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS gw_bulk_messages (
			job_id     TEXT NOT NULL,
			idx        INTEGER NOT NULL,
			to_jid     TEXT NOT NULL DEFAULT '',
			text       TEXT NOT NULL DEFAULT '',
			status     TEXT NOT NULL DEFAULT 'pending',
			message_id TEXT NOT NULL DEFAULT '',
			error      TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (job_id, idx),
			FOREIGN KEY (job_id) REFERENCES gw_bulk_jobs(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_bulk_jobs_status ON gw_bulk_jobs(status)`,
		`CREATE INDEX IF NOT EXISTS idx_gw_bulk_jobs_ts    ON gw_bulk_jobs(started_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("bulk schema: %w", err)
		}
	}
	return nil
}

// saveJob writes a new job and all its message rows (all "pending") atomically.
func (s *bulkStore) saveJob(ctx context.Context, job *BulkJob, msgs []BulkMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx,
		`INSERT INTO gw_bulk_jobs (id, session, status, total, sent, failed, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Session, job.Status, job.Total, 0, 0, job.StartedAt, 0)
	if err != nil {
		return fmt.Errorf("insert bulk job: %w", err)
	}

	for i, m := range msgs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO gw_bulk_messages (job_id, idx, to_jid, text, status, message_id, error)
			 VALUES (?, ?, ?, ?, 'pending', '', '')`,
			job.ID, i, m.To, m.Text); err != nil {
			return fmt.Errorf("insert bulk message %d: %w", i, err)
		}
	}
	return tx.Commit()
}

// updateMessage persists the result of a single send attempt.
func (s *bulkStore) updateMessage(jobID string, idx int, res BulkResult) {
	_, err := s.db.Exec(
		`UPDATE gw_bulk_messages SET status=?, message_id=?, error=? WHERE job_id=? AND idx=?`,
		res.Status, res.MessageID, res.Error, jobID, idx)
	if err != nil {
		s.log.Errorf("update bulk message %s[%d]: %v", jobID, idx, err)
	}
}

// updateJob persists job-level counters and status.
func (s *bulkStore) updateJob(job *BulkJob) {
	_, err := s.db.Exec(
		`UPDATE gw_bulk_jobs SET status=?, sent=?, failed=?, finished_at=? WHERE id=?`,
		job.Status, job.Sent, job.Failed, job.FinishedAt, job.ID)
	if err != nil {
		s.log.Errorf("update bulk job %s: %v", job.ID, err)
	}
}

// markInterrupted changes all "running" jobs to "interrupted" and sets finished_at.
// Called once on startup before any new jobs are accepted.
func (s *bulkStore) markInterrupted(ctx context.Context) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE gw_bulk_jobs SET status='interrupted', finished_at=? WHERE status='running'`, now)
	return err
}

// loadJob loads a job and its messages from DB.
func (s *bulkStore) loadJob(ctx context.Context, id string) (BulkJob, bool, error) {
	var j BulkJob
	err := s.db.QueryRowContext(ctx,
		`SELECT id, session, status, total, sent, failed, started_at, finished_at
		 FROM gw_bulk_jobs WHERE id=?`, id).
		Scan(&j.ID, &j.Session, &j.Status, &j.Total, &j.Sent, &j.Failed, &j.StartedAt, &j.FinishedAt)
	if err == sql.ErrNoRows {
		return BulkJob{}, false, nil
	}
	if err != nil {
		return BulkJob{}, false, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT to_jid, status, message_id, error FROM gw_bulk_messages
		 WHERE job_id=? ORDER BY idx`, id)
	if err != nil {
		return BulkJob{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var r BulkResult
		if err := rows.Scan(&r.To, &r.Status, &r.MessageID, &r.Error); err != nil {
			return BulkJob{}, false, err
		}
		j.Results = append(j.Results, r)
	}
	return j, true, rows.Err()
}

// listJobs loads recent jobs (newest first, max 200).
func (s *bulkStore) listJobs(ctx context.Context) ([]BulkJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session, status, total, sent, failed, started_at, finished_at
		 FROM gw_bulk_jobs ORDER BY started_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BulkJob
	for rows.Next() {
		var j BulkJob
		if err := rows.Scan(&j.ID, &j.Session, &j.Status, &j.Total, &j.Sent,
			&j.Failed, &j.StartedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ---- runner ----------------------------------------------------------------

// bulkRunner executes bulk jobs sequentially per job with delay + jitter.
type bulkRunner struct {
	mgr   *Manager
	log   waLog.Logger
	store *bulkStore

	mu   sync.Mutex
	jobs map[string]*BulkJob

	quit chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func newBulkRunner(mgr *Manager) *bulkRunner {
	return &bulkRunner{
		mgr:   mgr,
		log:   waLog.Stdout("Bulk", mgr.cfg.LogLevel, true),
		store: newBulkStore(mgr.db, waLog.Stdout("BulkStore", mgr.cfg.LogLevel, true)),
		jobs:  make(map[string]*BulkJob),
		quit:  make(chan struct{}),
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

	// Persist job + all messages (all "pending") before starting goroutine.
	if err := b.store.saveJob(context.Background(), job, msgs); err != nil {
		b.log.Errorf("persist bulk job: %v", err)
		// Continue even if persist fails — in-memory still works.
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

		// Persist per-message result immediately.
		b.store.updateMessage(job.ID, i, res)

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
	snap := job.snapshot()
	b.mu.Unlock()

	b.store.updateJob(&snap)
}

// job returns a snapshot by ID: from memory first, then from DB.
func (b *bulkRunner) job(id string) (BulkJob, bool) {
	b.mu.Lock()
	j, ok := b.jobs[id]
	if ok {
		snap := j.snapshot()
		b.mu.Unlock()
		return snap, true
	}
	b.mu.Unlock()

	// Fallback to DB (e.g. job from before restart).
	j2, found, err := b.store.loadJob(context.Background(), id)
	if err != nil {
		b.log.Errorf("load bulk job %s: %v", id, err)
		return BulkJob{}, false
	}
	return j2, found
}

// list returns snapshots of all jobs (memory + DB), newest first, deduplicated.
func (b *bulkRunner) list() []BulkJob {
	b.mu.Lock()
	memJobs := make(map[string]BulkJob, len(b.jobs))
	for id, j := range b.jobs {
		memJobs[id] = j.snapshot()
	}
	b.mu.Unlock()

	dbJobs, err := b.store.listJobs(context.Background())
	if err != nil {
		b.log.Errorf("list bulk jobs from db: %v", err)
	}

	// Merge: memory takes precedence over DB for same ID (more up-to-date).
	seen := make(map[string]bool)
	var out []BulkJob
	for _, j := range dbJobs {
		if mem, ok := memJobs[j.ID]; ok {
			out = append(out, mem)
		} else {
			out = append(out, j)
		}
		seen[j.ID] = true
	}
	// Add in-memory jobs not yet flushed to DB (shouldn't happen, but safe).
	for id, j := range memJobs {
		if !seen[id] {
			out = append(out, j)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// cleanupLocked removes finished jobs older than one hour from memory only.
// DB records are kept for audit. Caller holds b.mu.
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
