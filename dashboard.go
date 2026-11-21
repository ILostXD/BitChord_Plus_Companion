package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	_ "embed"
)

//go:embed dashboard.html
var dashboardHTML []byte

// The dashboard page holds no data of its own: it signs in with the Navidrome account in
// the browser and reads /api/v1/acquisition/jobs like the app does, so serving it needs no auth.
func serveDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:")
	_, _ = w.Write(dashboardHTML)
}

// decide answers a job that is waiting on a person: {"action":"download"} fetches it anyway,
// {"action":"skip"} keeps what the library has.
func (a *acquisitionService) decide(w http.ResponseWriter, r *http.Request) {
	var input struct{ Action string }
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if json.NewDecoder(r.Body).Decode(&input) != nil || (input.Action != "download" && input.Action != "skip") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	a.mu.Lock()
	job := a.jobs[r.PathValue("jobId")]
	if job == nil {
		a.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job_not_found"})
		return
	}
	if job.Status != "needs_review" || job.Error != "library_review" {
		a.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not_waiting_for_a_decision"})
		return
	}
	snapshot := *job
	snapshot.UpdatedAt = time.Now().UTC()
	if input.Action == "skip" {
		snapshot.Status, snapshot.Error = "skipped", ""
	} else {
		snapshot.Status, snapshot.Error, snapshot.Forced = "searching", "", true
	}
	err := a.updateLocked(&snapshot)
	// Old duplicates of the same request are answered with it, so one answer is enough.
	for _, other := range a.jobs {
		if other.ID != snapshot.ID && other.Status == "needs_review" && other.Error == "library_review" &&
			strings.EqualFold(other.Artist, snapshot.Artist) && strings.EqualFold(other.Title, snapshot.Title) && other.Kind == snapshot.Kind {
			twin := *other
			twin.Status, twin.Error, twin.UpdatedAt = "skipped", "", time.Now().UTC()
			_ = a.updateLocked(&twin)
		}
	}
	a.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "job_storage_failed"})
		return
	}
	if input.Action == "download" {
		go a.run(snapshot.ID)
	}
	writeJSON(w, http.StatusOK, snapshot)
}
