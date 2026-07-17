package gateway

import (
	"context"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func newTestBulkStore(t *testing.T) *bulkStore {
	t.Helper()
	db := testDB(t)

	s := newBulkStore(db, waLog.Noop)
	if err := s.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if _, err := db.Exec(`TRUNCATE gw_bulk_messages, gw_bulk_jobs`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// TestBulkResumeOnlyPending verifies the safe-resume contract: after a crash,
// loadResumable returns ONLY recipients that were never attempted (status
// "pending"). Already sent/failed recipients must not be re-sent.
func TestBulkResumeOnlyPending(t *testing.T) {
	s := newTestBulkStore(t)
	ctx := context.Background()

	job := &BulkJob{ID: "job1", Session: "default", Status: "running", Total: 4}
	msgs := []BulkMessage{
		{To: "628001", Text: "a"},
		{To: "628002", Text: "b"},
		{To: "628003", Text: "c"},
		{To: "628004", Text: "d"},
	}
	if err := s.saveJob(ctx, job, msgs, 1000, 2000); err != nil {
		t.Fatalf("saveJob: %v", err)
	}

	// Simulate progress before a crash: idx0 sent, idx1 failed, idx2/idx3 pending.
	s.updateMessage("job1", 0, BulkResult{To: "628001", Status: "sent", MessageID: "m0"})
	s.updateMessage("job1", 1, BulkResult{To: "628002", Status: "failed", Error: "boom"})

	resumable, err := s.loadResumable(ctx)
	if err != nil {
		t.Fatalf("loadResumable: %v", err)
	}
	if len(resumable) != 1 {
		t.Fatalf("expected 1 resumable job, got %d", len(resumable))
	}

	rj := resumable[0]
	if rj.minDelay != 1000 || rj.maxDelay != 2000 {
		t.Errorf("delays not preserved: got min=%d max=%d", rj.minDelay, rj.maxDelay)
	}
	if rj.job.Sent != 1 || rj.job.Failed != 1 {
		t.Errorf("counts wrong: sent=%d failed=%d (want 1/1)", rj.job.Sent, rj.job.Failed)
	}

	// Only idx2 and idx3 must be pending.
	if len(rj.pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(rj.pending))
	}
	gotIdx := map[int]string{}
	for _, it := range rj.pending {
		gotIdx[it.idx] = it.to
	}
	if gotIdx[2] != "628003" || gotIdx[3] != "628004" {
		t.Errorf("pending recipients wrong: %v", gotIdx)
	}

	// Full Results must reflect all 4 statuses in order.
	loaded, found, err := s.loadJob(ctx, "job1")
	if err != nil || !found {
		t.Fatalf("loadJob: found=%v err=%v", found, err)
	}
	wantStatus := []string{"sent", "failed", "pending", "pending"}
	if len(loaded.Results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(loaded.Results))
	}
	for i, w := range wantStatus {
		if loaded.Results[i].Status != w {
			t.Errorf("result[%d] status = %q, want %q", i, loaded.Results[i].Status, w)
		}
	}
}

// TestBulkResumeNoneWhenCompleted ensures a fully-sent job exposes no pending work.
func TestBulkResumeNoneWhenCompleted(t *testing.T) {
	s := newTestBulkStore(t)
	ctx := context.Background()

	job := &BulkJob{ID: "job2", Session: "default", Status: "running", Total: 2}
	msgs := []BulkMessage{{To: "628001", Text: "a"}, {To: "628002", Text: "b"}}
	if err := s.saveJob(ctx, job, msgs, 0, 0); err != nil {
		t.Fatalf("saveJob: %v", err)
	}
	s.updateMessage("job2", 0, BulkResult{To: "628001", Status: "sent"})
	s.updateMessage("job2", 1, BulkResult{To: "628002", Status: "sent"})

	resumable, err := s.loadResumable(ctx)
	if err != nil {
		t.Fatalf("loadResumable: %v", err)
	}
	if len(resumable) != 1 {
		t.Fatalf("expected 1 job row, got %d", len(resumable))
	}
	if len(resumable[0].pending) != 0 {
		t.Errorf("expected 0 pending for completed-in-fact job, got %d", len(resumable[0].pending))
	}
}
