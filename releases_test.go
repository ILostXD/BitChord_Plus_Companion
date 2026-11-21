package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExplicitApplePresaveUsesVerifiedCatalogueAndPreservesSavedState(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	requests := 0
	a := &acquisitionService{cfg: config{downloadPath: t.TempDir(), navidromeUsername: "owner"}, jobs: map[string]*acquisitionJob{}}
	a.client = &http.Client{Transport: catalogueTestTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "itunes.apple.com" || r.URL.Path != "/lookup" || r.URL.Query().Get("id") != "42" {
			t.Fatalf("unexpected lookup: %s", r.URL)
		}
		body := `{"results":[{"wrapperType":"collection","collectionId":42,"artistName":"Artist","collectionName":"Future Album","releaseDate":"` + future + `","trackCount":1},{"kind":"song","collectionId":42,"trackId":7,"trackName":"Future Track"}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	s := &releaseService{a: a, path: filepath.Join(a.cfg.downloadPath, "upcoming.json"), data: releaseData{Albums: map[string]upcomingRelease{}, Checked: map[string]time.Time{}, Saves: map[string]map[string]bool{}}}
	if err := s.registerCatalogue(context.Background(), "../42"); err == nil || requests != 0 {
		t.Fatal("untrusted ID reached catalogue")
	}
	if err := s.registerCatalogue(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	album := s.data.Albums["42"]
	if album.ArtistID != "" || album.Title != "Future Album" || len(album.Tracks) != 1 || album.Saved {
		t.Fatalf("incorrect explicit release: %+v", album)
	}
	s.setSaved("owner", "42", true)
	if err := s.registerCatalogue(context.Background(), "42"); err != nil || requests != 1 || !s.userAlbum(s.data.Albums["42"], "owner").Saved {
		t.Fatal("cached refresh lost save or repeated lookup")
	}
	if s.userAlbum(s.data.Albums["42"], "other").Saved {
		t.Fatal("save leaked to another account")
	}
}

func TestReleaseFavoritesPresaveAndCatalogue(t *testing.T) {
	now := time.Now()
	future := now.Add(48 * time.Hour)
	entry := itunesEntry{CollectionID: 42, ArtistID: 7, ArtistName: "Quavo", CollectionName: "QRÖMELIFE", ReleaseDate: future.UTC().Format(time.RFC3339), TrackCount: 2, ArtworkURL100: "https://is1-ssl.mzstatic.com/cover/100x100bb.jpg"}
	album := catalogueAlbum(entry, []itunesEntry{
		{Kind: "song", CollectionID: 42, TrackID: 2, TrackNumber: 2, TrackName: "Pending"},
		{Kind: "song", CollectionID: 42, TrackID: 1, TrackNumber: 1, TrackName: "Single", ReleaseDate: now.Add(-time.Hour).UTC().Format(time.RFC3339), PreviewURL: "https://audio-ssl.itunes.apple.com/single.m4a", IsStreamable: true},
		{Kind: "song", CollectionID: 99, TrackID: 3, TrackName: "Wrong album"},
	}, "nav-artist", now)
	if album.Tracks[0].Title != "Single" || !album.Tracks[0].Released || album.Tracks[1].Released || len(album.Tracks) != 2 || !strings.Contains(album.Cover, "800x800bb") || album.ReleaseAt != time.Date(future.UTC().Year(), future.UTC().Month(), future.UTC().Day(), 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("bad catalogue metadata: %+v", album)
	}
	entry.ArtworkURL100 = "https://private-server/cover.jpg"
	if catalogueAlbum(entry, nil, "nav-artist", now).Cover != "" {
		t.Fatal("untrusted artwork accepted")
	}
	followed := true
	nav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artists := `[]`
		if followed {
			artists = `[{"id":"nav-artist","name":"Quavo"}]`
		}
		_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok","starred2":{"artist":` + artists + `}}}`))
	}))
	defer nav.Close()
	a := &acquisitionService{cfg: config{navidromeURL: nav.URL, navidromeUsername: "owner", navidromePassword: "password", downloadPath: t.TempDir()}, client: nav.Client(), jobs: map[string]*acquisitionJob{}}
	s := &releaseService{a: a, path: filepath.Join(a.cfg.downloadPath, "upcoming.json"), data: releaseData{Albums: map[string]upcomingRelease{album.ID: album}, Checked: map[string]time.Time{}}, wake: make(chan struct{}, 1)}
	s.initializeSaves()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/releases", s.list)
	mux.HandleFunc("PUT /api/v1/releases/{id}/presave", s.save)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	if w := call("GET", "/api/v1/releases", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"followed":true`) {
		t.Fatalf("favorite feed: %s", w.Body)
	}
	if w := call("PUT", "/api/v1/releases/42/presave", `{"saved":true} {}`); w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
	if w := call("PUT", "/api/v1/releases/42/presave", `{"saved":true}`); w.Code != 200 {
		t.Fatalf("pre-save: %s", w.Body)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var restored releaseData
	if json.Unmarshal(raw, &restored) != nil || !restored.Saves["owner"]["42"] || restored.Albums["42"].Saved {
		t.Fatal("pre-save not durable")
	}
	followed = false
	if w := call("GET", "/api/v1/releases", ""); !strings.Contains(w.Body.String(), `"followed":false`) || !strings.Contains(w.Body.String(), `"saved":true`) {
		t.Fatal("unfavorite lost an explicit pre-save")
	}
	if w := call("PUT", "/api/v1/releases/42/presave", `{"saved":false}`); w.Code != 200 {
		t.Fatal("cannot undo pre-save")
	}
	if w := call("GET", "/api/v1/releases", ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("unfollowed artist leaked into feed")
	}
	s.setSaved("owner", album.ID, true)
	album.ReleaseAt = now.Add(-time.Hour).UnixMilli()
	album.JobID = "existing"
	s.data.Albums["42"] = album
	a.jobs["existing"] = &acquisitionJob{ID: "existing", Status: "rescanned"}
	s.acquireDue() // Completed saved releases must never be submitted again.
	if len(a.jobs) != 1 || s.jobState(s.data.Albums["42"]).Status != "rescanned" {
		t.Fatal("completed pre-save resubmitted")
	}
	album.JobID = ""
	s.data.Albums["42"] = album
	a.jobs["existing"] = &acquisitionJob{ID: "existing", Artist: album.Artist, Title: album.Title, Kind: "album", Status: "rescanned"}
	s.acquireDue()
	if s.data.Albums["42"].JobID != "existing" || len(a.jobs) != 1 {
		t.Fatal("due pre-save bypassed acquisition deduplication")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handler := newHandler(strings.Repeat("a", 32), ctx, a.cfg)
	for _, path := range []string{"/api/v1/releases", "/api/v1/releases/42", "/api/v1/releases/42/presave"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal("unauthenticated release route accessible")
		}
	}
}

func TestCatalogueReleaseUsesDateNotAnAssumedLaunchHour(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-10-02T07:10:00+02:00")
	entry := itunesEntry{CollectionID: 42, ReleaseDate: "2026-10-02T07:00:00Z"}
	tracks := []itunesEntry{{Kind: "song", CollectionID: 42, TrackID: 1, PreviewURL: "https://audio-ssl.itunes.apple.com/preview.m4a", IsStreamable: true}}
	album := catalogueAlbum(entry, tracks, "artist", now)
	if album.ReleaseAt > now.UnixMilli() || album.Date != "2026-10-02" || !album.Tracks[0].Released {
		t.Fatalf("catalogue time treated as launch hour: %+v", album)
	}
	entry.ReleaseDate = "2026-10-03T07:00:00Z"
	if catalogueAlbum(entry, tracks, "artist", now).Tracks[0].Released {
		t.Fatal("preorder preview treated as released track")
	}
	tracks[0].ReleaseDate = "2026-10-01T12:00:00Z"
	if !catalogueAlbum(entry, tracks, "artist", now).Tracks[0].Released {
		t.Fatal("released single hidden")
	}
	tracks[0].IsStreamable = false
	if catalogueAlbum(entry, tracks, "artist", now).Tracks[0].Released {
		t.Fatal("preview alone treated as streamable")
	}
}
