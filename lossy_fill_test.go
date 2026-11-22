package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFillLossyOnlyTakesJioSaavnForFailedSongs(t *testing.T) {
	a := &acquisitionService{cfg: config{downloadPath: t.TempDir(), libraryPath: t.TempDir()}, jobs: map[string]*acquisitionJob{}}
	id := strings.Repeat("c", 32)
	_ = a.update(&acquisitionJob{ID: id, Artist: "A", Title: "T", Kind: "track", Status: "failed", Error: "no_suitable_slskd_match"})
	album := strings.Repeat("d", 32)
	_ = a.update(&acquisitionJob{ID: album, Artist: "A", Title: "T", Kind: "album", Status: "failed", Error: "catalogue_tracklist_unavailable"})
	post := func(job, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.SetPathValue("jobId", job)
		a.fillLossy(w, r)
		return w.Code
	}
	for _, u := range []string{"http://aac.saavncdn.com/x.mp4", "https://evil.example/x.mp4", "https://saavncdn.com.evil.example/x.mp4", "https://127.0.0.1/x"} {
		if code := post(id, `{"url":"`+u+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s accepted (%d)", u, code)
		}
	}
	if code := post(album, `{"url":"https://aac.saavncdn.com/x.mp4","title":"T","track":1}`); code != http.StatusConflict {
		t.Errorf("a request that failed for another reason was accepted (%d)", code)
	}
}
