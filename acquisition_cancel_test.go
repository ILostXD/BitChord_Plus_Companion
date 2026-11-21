package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCancellationStopsOwnedTransferAndSurvivesStaleWorkerUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := strings.Repeat("a", 32)
	var stopped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE":
			stopped.Store(true)
			w.WriteHeader(200)
		case r.URL.Path == "/api/v0/transfers/downloads/batches/batch":
			state := "InProgress"
			if stopped.Load() {
				state = "Completed, Cancelled"
			}
			writeJSON(w, 200, map[string]any{"id": "batch", "username": "peer", "options": map[string]string{"destination": "glaze/" + id},
				"transfers": []any{map[string]string{"id": "file", "filename": "01.flac", "state": state}}})
		default:
			t.Errorf("cancellation tried another source: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	a := &acquisitionService{ctx: ctx, client: server.Client(), cfg: config{downloadPath: root, libraryPath: filepath.Join(root, "music"), slskdURL: server.URL, slskdAPIKey: "key"}, jobs: map[string]*acquisitionJob{}}
	j := acquisitionJob{ID: id, Artist: "Artist", Title: "Album", Kind: "album", Status: "downloading", Source: "slskd", BatchID: "batch", Username: "peer", Files: []slskFile{{Filename: "01.flac", Size: 10}}}
	if err := a.update(&j); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "glaze", id)
	_ = os.Mkdir(directory, 0700)
	_ = os.WriteFile(filepath.Join(directory, "01.flac"), []byte("partial"), 0600)
	unrelated := filepath.Join(root, "other.flac")
	_ = os.WriteFile(unrelated, []byte("keep"), 0600)
	workerStopped := false
	a.workers = map[string]context.CancelFunc{id: func() { workerStopped = true }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs/{jobId}/cancel", a.cancelJob)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/jobs/"+id+"/cancel", nil))
	if w.Code != 202 || !workerStopped {
		t.Fatalf("cancel failed: %d %s", w.Code, w.Body)
	}
	j.Status = "downloading" // A worker finishing an earlier HTTP request cannot erase the stop request.
	if err := a.update(&j); err != nil || !j.CancelRequested || j.Status != "cancelling" {
		t.Fatal("stale worker undid cancellation")
	}
	delete(a.workers, id)
	a.run(id)
	if a.jobs[id].Status != "cancelled" || !stopped.Load() {
		t.Fatal("transfer did not stop")
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("job's staging was not cleaned")
	}
	if raw, _ := os.ReadFile(unrelated); string(raw) != "keep" {
		t.Fatal("unrelated file touched")
	}
	data, _ := os.ReadFile(filepath.Join(root, "glaze", "jobs", id+".json"))
	var restored acquisitionJob
	_ = json.Unmarshal(data, &restored)
	if restored.Status != "cancelled" || !restored.CancelRequested {
		t.Fatal("cancellation not durable")
	}
	a.run(id) // Never restart or fall back after completion.
	j.Status, j.CancelRequested = "importing", false
	a.jobs[id] = &j
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/jobs/"+id+"/cancel", nil))
	if w.Code != 409 {
		t.Fatal("accepted cancellation after import boundary")
	}
}

func TestCancelledPresaveDoesNotRestartAndRetryTimesAreReported(t *testing.T) {
	now := time.Now()
	a := &acquisitionService{cfg: config{downloadPath: t.TempDir()}, jobs: map[string]*acquisitionJob{"job": {ID: "job", Status: "cancelled", CancelRequested: true, UpdatedAt: now}}}
	s := &releaseService{a: a, data: releaseData{Albums: map[string]upcomingRelease{"42": {ID: "42", JobID: "job", ReleaseAt: now.Add(-time.Hour).UnixMilli()}}, Saves: map[string]map[string]bool{"owner": {"42": true}}}}
	s.acquireDue()
	if len(a.jobs) != 1 || a.jobs["job"].Status != "cancelled" {
		t.Fatal("cancelled pre-save restarted")
	}
	a.jobs["job"].Status = "failed"
	album := s.jobState(s.data.Albums["42"])
	if album.CheckedAt != now.UnixMilli() || album.NextCheckAt != now.Add(time.Hour).UnixMilli() {
		t.Fatal("retry time missing")
	}
	if releaseRetryInterval(now.Add(-48*time.Hour).UnixMilli(), now) != 24*time.Hour {
		t.Fatal("daily retry schedule changed")
	}
	a.jobs["job"].Status = "downloading"
	if s.jobState(album).NextCheckAt != 0 {
		t.Fatal("active download retained an obsolete retry time")
	}
}
