package gateway

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
			finished_at  INTEGER NOT NULL DEFAULT 0,
			min_delay_ms INTEGER NOT NULL DEFAULT 0,
			max_delay_ms INTEGER NOT NULL DEFAULT 0
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
	// Migrasi DB lama: tambah kolom delay (abaikan error "duplicate column").
	for _, col := range []string{
		`ALTER TABLE gw_bulk_jobs ADD COLUMN min_delay_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE gw_bulk_jobs ADD COLUMN max_delay_ms INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.ExecContext(ctx, col); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				s.log.Debugf("alter gw_bulk_jobs: %v", err)
			}
		}
	}
	return nil
}

// saveJob writes a new job and all its message rows (all "pending") atomically.
func (s *bulkStore) saveJob(ctx context.Context, job *BulkJob, msgs []BulkMessage, minD, maxD int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx,
		`INSERT INTO gw_bulk_jobs (id, session, status, total, sent, failed, started_at, finished_at, min_delay_ms, max_delay_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Session, job.Status, job.Total, 0, 0, job.StartedAt, 0, minD, maxD)
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
// Used when auto-resume is disabled.
func (s *bulkStore) markInterrupted(ctx context.Context) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE gw_bulk_jobs SET status='interrupted', finished_at=? WHERE status='running'`, now)
	return err
}

// bulkItem is a single recipient to send, with its persisted row index.
type bulkItem struct {
	idx  int
	to   string
	text string
}

// resumableJob bundles a job, its already-loaded full results, the still-pending
// recipients, and the original delay settings.
type resumableJob struct {
	job      BulkJob
	pending  []bulkItem
	minDelay int
	maxDelay int
}

// loadResumable returns jobs that were running or interrupted (i.e. not finished),
// each with the list of recipients still "pending" (never attempted).
func (s *bulkStore) loadResumable(ctx context.Context) ([]resumableJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session, status, total, started_at, min_delay_ms, max_delay_ms
		 FROM gw_bulk_jobs WHERE status IN ('running','interrupted') ORDER BY started_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type head struct {
		id        string
		session   string
		total     int
		startedAt int64
		minD      int
		maxD      int
	}
	var heads []head
	for rows.Next() {
		var h head
		var status string
		if err := rows.Scan(&h.id, &h.session, &status, &h.total, &h.startedAt, &h.minD, &h.maxD); err != nil {
			return nil, err
		}
		heads = append(heads, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []resumableJob
	for _, h := range heads {
		job := BulkJob{
			ID:        h.id,
			Session:   h.session,
			Status:    "running",
			Total:     h.total,
			StartedAt: h.startedAt,
			Results:   make([]BulkResult, h.total),
		}
		mrows, err := s.db.QueryContext(ctx,
			`SELECT idx, to_jid, text, status, message_id, error FROM gw_bulk_messages
			 WHERE job_id=? ORDER BY idx`, h.id)
		if err != nil {
			return nil, err
		}
		var pending []bulkItem
		for mrows.Next() {
			var idx int
			var to, text, status, mid, errStr string
			if err := mrows.Scan(&idx, &to, &text, &status, &mid, &errStr); err != nil {
				mrows.Close()
				return nil, err
			}
			if idx >= 0 && idx < len(job.Results) {
				job.Results[idx] = BulkResult{To: to, Status: status, MessageID: mid, Error: errStr}
			}
			switch status {
			case "sent":
				job.Sent++
			case "failed":
				job.Failed++
			case "pending":
				pending = append(pending, bulkItem{idx: idx, to: to, text: text})
			}
		}
		mrows.Close()
		if err := mrows.Err(); err != nil {
			return nil, err
		}
		out = append(out, resumableJob{job: job, pending: pending, minDelay: h.minD, maxDelay: h.maxD})
	}
	return out, nil
}

// loadJob loads a job and its messages from DB. Sent/Failed are recomputed from
// message rows so the counts are always accurate.
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
	var sent, failed int
	for rows.Next() {
		var r BulkResult
		if err := rows.Scan(&r.To, &r.Status, &r.MessageID, &r.Error); err != nil {
			return BulkJob{}, false, err
		}
		switch r.Status {
		case "sent":
			sent++
		case "failed":
			failed++
		}
		j.Results = append(j.Results, r)
	}
	if len(j.Results) > 0 {
		j.Sent, j.Failed = sent, failed
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
		Results:   make([]BulkResult, len(msgs)),
	}
	items := make([]bulkItem, len(msgs))
	for i, m := range msgs {
		job.Results[i] = BulkResult{To: m.To, Status: "pending"}
		items[i] = bulkItem{idx: i, to: m.To, text: m.Text}
	}

	// Persist job + all messages (all "pending") before starting goroutine.
	if err := b.store.saveJob(context.Background(), job, msgs, minD, maxD); err != nil {
		b.log.Errorf("persist bulk job: %v", err)
		// Continue even if persist fails — in-memory still works.
	}

	b.mu.Lock()
	b.cleanupLocked()
	b.jobs[job.ID] = job
	b.mu.Unlock()

	b.wg.Add(1)
	go b.run(job, session, items, minD, maxD)

	return job.snapshot(), nil
}

// run sends each item sequentially, sleeping a jittered delay between sends.
// Results are updated in place by their persisted index, so it works for both
// fresh jobs and resumed jobs (which only process the still-pending subset).
func (b *bulkRunner) run(job *BulkJob, session string, items []bulkItem, minD, maxD int) {
	defer b.wg.Done()

	for n, it := range items {
		select {
		case <-b.quit:
			// Graceful shutdown: leave remaining as pending, mark interrupted so
			// the job resumes on next startup.
			b.finish(job, "interrupted")
			return
		default:
		}

		res := BulkResult{To: it.to, Status: "sent"}
		sess, err := b.mgr.Get(session)
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			id, sErr := sess.SendText(ctx, it.to, it.text)
			cancel()
			if sErr != nil {
				res.Status, res.Error = "failed", sErr.Error()
			} else {
				res.MessageID = id
			}
		}

		// Persist per-message result immediately.
		b.store.updateMessage(job.ID, it.idx, res)

		b.mu.Lock()
		if it.idx >= 0 && it.idx < len(job.Results) {
			job.Results[it.idx] = res
		}
		if res.Status == "sent" {
			job.Sent++
		} else {
			job.Failed++
		}
		b.mu.Unlock()

		if n < len(items)-1 {
			if !b.sleepJitter(minD, maxD) {
				b.finish(job, "interrupted")
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

// startResume kicks off recovery of unfinished jobs in the background. Call once
// at startup, after sessions have begun connecting.
func (b *bulkRunner) startResume() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.resumeInterrupted(context.Background())
	}()
}

// resumeInterrupted recovers jobs left "running"/"interrupted" by a crash or
// restart. With auto-resume enabled (safe mode), it re-sends only the recipients
// that were never attempted (status "pending"). Already-sent recipients are
// skipped, so there are no duplicates except in the rare window where a send
// reached WhatsApp but the status write did not survive the crash.
func (b *bulkRunner) resumeInterrupted(ctx context.Context) {
	if !b.mgr.cfg.BulkAutoResume {
		if err := b.store.markInterrupted(ctx); err != nil {
			b.log.Errorf("mark interrupted bulk jobs: %v", err)
		}
		return
	}

	jobs, err := b.store.loadResumable(ctx)
	if err != nil {
		b.log.Errorf("load resumable bulk jobs: %v", err)
		return
	}

	for _, rj := range jobs {
		select {
		case <-b.quit:
			return
		default:
		}

		// Nothing left to send → finalize as completed.
		if len(rj.pending) == 0 {
			jc := rj.job
			jc.Status = "completed"
			jc.FinishedAt = time.Now().Unix()
			b.store.updateJob(&jc)
			continue
		}

		// Wait for the session to be ready; if it never connects, leave the job
		// interrupted so the operator can retry later (we don't burn pending as failed).
		if !b.waitReady(rj.job.Session, 30*time.Second) {
			jc := rj.job
			jc.Status = "interrupted"
			jc.FinishedAt = time.Now().Unix()
			b.store.updateJob(&jc)
			b.log.Warnf("skip resume bulk job %s: session %q not ready", jc.ID, jc.Session)
			continue
		}

		job := rj.job // copy
		job.Status = "running"
		job.FinishedAt = 0
		jp := &job

		b.mu.Lock()
		b.jobs[jp.ID] = jp
		b.mu.Unlock()
		b.store.updateJob(jp) // mark running again, clear finished_at

		b.log.Infof("resuming bulk job %s: %d pending of %d", jp.ID, len(rj.pending), jp.Total)
		b.wg.Add(1)
		go b.run(jp, jp.Session, rj.pending, rj.minDelay, rj.maxDelay)
	}
}

// waitReady blocks until the named session is connected+logged in, or timeout.
func (b *bulkRunner) waitReady(session string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if sess, err := b.mgr.Get(session); err == nil && sess.IsReady() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-b.quit:
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
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
