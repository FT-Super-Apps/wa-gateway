package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"wa-gateway/pkg/version"
)

func TestHandleVersion(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleVersion(rec, httptest.NewRequest(http.MethodGet, "/version", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got version.Info
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := version.Get()
	if got != want {
		t.Fatalf("body = %+v, want %+v", got, want)
	}
}

func TestHandleHealthIncludesVersion(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "ok" || got["version"] != version.Version || got["build_number"] != version.BuildNumber {
		t.Fatalf("unexpected health body: %v", got)
	}
}
