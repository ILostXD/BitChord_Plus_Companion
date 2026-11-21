package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompanionAccountAuthorization(t *testing.T) {
	nav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("u")
		if r.URL.Query().Get("t") != strings.Repeat("b", 32) || (user != "Andy" && user != "brother") {
			writeJSON(w, 200, map[string]any{"subsonic-response": map[string]string{"status": "failed"}})
			return
		}
		if strings.Contains(r.URL.Path, "getUser") {
			if r.URL.Query().Get("username") != user {
				t.Error("identity lookup queried another account")
			}
			writeJSON(w, 200, map[string]any{"subsonic-response": map[string]any{"status": "ok", "user": map[string]string{"username": user}}})
		} else {
			_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok","starred2":{"artist":[]}}}`))
		}
	}))
	defer nav.Close()
	handler := newHandler(strings.Repeat("x", 32), t.Context(), config{navidromeURL: nav.URL, navidromeUsername: "andy"})
	for _, path := range []string{"jobs", "jobs/old/resume", "jobs/old/approve", "jobs/old/review"} {
		r := httptest.NewRequest("POST", "/api/v1/tagging/"+path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "smart_tagging_removed") {
			t.Fatal("retired write route still available", path, w.Code)
		}
	}
	for _, tc := range []struct {
		user, token, path string
		want              int
	}{
		{"brother", "b", "/api/v1/releases", 200},
		{"brother", "b", "/api/v1/storage", 503},
		{"brother", "b", "/api/v1/acquisition/jobs", 200},
		{"brother", "b", "/api/v1/tagging/jobs", 401},
		{"brother", "b", "/health", 401},
		{"brother", "c", "/api/v1/releases", 401},
		{"unknown", "b", "/api/v1/storage", 401},
		{"Andy", "b", "/api/v1/tagging/jobs/not-real/report", 410},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("X-Jam-Navidrome-User", tc.user)
		r.Header.Set("X-Jam-Navidrome-Salt", strings.Repeat("a", 24))
		r.Header.Set("X-Jam-Navidrome-Token", strings.Repeat(tc.token, 32))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.user, tc.path, w.Code, w.Body.String())
		}
	}
	for _, action := range []string{"approve", "review", "rescan"} {
		r := httptest.NewRequest("POST", "/api/v1/tagging/jobs/not-real/"+action, strings.NewReader(`{}`))
		r.Header.Set("X-Jam-Navidrome-User", "brother")
		r.Header.Set("X-Jam-Navidrome-Salt", strings.Repeat("a", 24))
		r.Header.Set("X-Jam-Navidrome-Token", strings.Repeat("b", 32))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("another account reached shared-library write controls", action, w.Code)
		}
	}
}

func TestReleaseAccountsKeepFavoritesAndSavesSeparate(t *testing.T) {
	nav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "owner-artist"
		if r.URL.Query().Get("u") == "brother" {
			id = "brother-artist"
		}
		_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok","starred2":{"artist":[{"id":"` + id + `","name":"Artist"}]}}}`))
	}))
	defer nav.Close()
	a := &acquisitionService{cfg: config{navidromeURL: nav.URL, navidromeUsername: "owner", navidromePassword: "password", downloadPath: t.TempDir()}, client: nav.Client(), jobs: map[string]*acquisitionJob{}}
	s := &releaseService{a: a, path: filepath.Join(a.cfg.downloadPath, "upcoming.json"), wake: make(chan struct{}, 1), data: releaseData{
		Albums: map[string]upcomingRelease{
			"1": {ID: "1", ArtistID: "owner-artist", ReleaseAt: time.Now().Add(time.Hour).UnixMilli(), Saved: true},
			"2": {ID: "2", ArtistID: "brother-artist", ReleaseAt: time.Now().Add(2 * time.Hour).UnixMilli()},
		}, Checked: map[string]time.Time{},
	}}
	s.initializeSaves()
	call := func(user, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r = r.WithContext(context.WithValue(r.Context(), navidromeIdentityKey{}, navidromeIdentity{User: user, Salt: strings.Repeat("a", 24), Token: strings.Repeat("b", 32)}))
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v1/releases", s.list)
		mux.HandleFunc("GET /api/v1/releases/{id}", s.detail)
		mux.HandleFunc("PUT /api/v1/releases/{id}/presave", s.save)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	feed := func(user string) []upcomingRelease {
		w := call(user, "GET", "/api/v1/releases", "")
		var albums []upcomingRelease
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &albums) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return albums
	}
	brother := feed("brother")
	if len(brother) != 1 || brother[0].ID != "2" || !brother[0].Followed || brother[0].Saved {
		t.Fatal("owner favorites/saves leaked", brother)
	}
	if s.pending["brother-artist"].ID == "" {
		t.Fatal("brother favorites never queued for catalogue refresh")
	}
	if owner := feed("owner"); len(owner) != 1 || owner[0].ID != "1" || !owner[0].Saved {
		t.Fatal("legacy owner save lost", owner)
	}
	if w := call("brother", "PUT", "/api/v1/releases/1/presave", `{"saved":true}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call("owner", "PUT", "/api/v1/releases/1/presave", `{"saved":false}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if !s.anySaved("1") || s.data.Saves["owner"]["1"] || !s.data.Saves["brother"]["1"] {
		t.Fatal("one user cancelled another's save")
	}
	var ownerDetail, brotherDetail upcomingRelease
	_ = json.Unmarshal(call("owner", "GET", "/api/v1/releases/1", "").Body.Bytes(), &ownerDetail)
	_ = json.Unmarshal(call("brother", "GET", "/api/v1/releases/1", "").Body.Bytes(), &brotherDetail)
	if ownerDetail.Saved || !brotherDetail.Saved {
		t.Fatal("detail leaked save state")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var restored releaseData
	if json.Unmarshal(raw, &restored) != nil || !restored.Saves["brother"]["1"] || strings.Contains(string(raw), strings.Repeat("b", 32)) {
		t.Fatal("saves not durable or credentials stored")
	}
	loaded := &releaseService{a: a, data: restored}
	loaded.initializeSaves()
	if !loaded.data.Saves["brother"]["1"] || loaded.data.Saves["owner"]["1"] {
		t.Fatal("restart reassigned a save to another account")
	}
	oldPath := s.path
	s.path = t.TempDir() // A directory cannot be replaced by the JSON file.
	if w := call("brother", "PUT", "/api/v1/releases/1/presave", `{"saved":false}`); w.Code != 500 || !s.data.Saves["brother"]["1"] {
		t.Fatal("failed persistence lost a save")
	}
	s.path = oldPath
	// Either user's save can acquire once; the library and acquisition jobs are shared.
	album := s.data.Albums["1"]
	album.ReleaseAt = time.Now().Add(-time.Hour).UnixMilli()
	s.data.Albums["1"] = album
	a.jobs["existing"] = &acquisitionJob{ID: "existing", Artist: album.Artist, Title: album.Title, Kind: "album", Status: "rescanned"}
	// Use real names so the normal enqueue validator and deduplication are exercised.
	album.Artist, album.Title = "Artist", "Album"
	s.data.Albums["1"] = album
	a.jobs["existing"].Artist, a.jobs["existing"].Title = album.Artist, album.Title
	s.acquireDue()
	s.acquireDue()
	if s.data.Albums["1"].JobID != "existing" || len(a.jobs) != 1 {
		t.Fatal("multi-user pre-save downloaded twice")
	}
}
