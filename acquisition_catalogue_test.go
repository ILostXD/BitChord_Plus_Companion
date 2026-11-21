package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCataloguePreviewAndAcquisitionShareCompleteRelease(t *testing.T) {
	requests := 0
	a := &acquisitionService{ctx: context.Background(), cfg: config{downloadPath: t.TempDir()}, jobs: make(map[string]*acquisitionJob)}
	a.client = &http.Client{Transport: catalogueTestTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "api.deezer.com" {
			t.Fatalf("unexpected source: %s", r.URL)
		}
		var body string
		switch r.URL.Path {
		case "/album/42":
			body = `{"id":42,"title":"Oxymoron (Deluxe)","artist":{"name":"Schoolboy Q"},"nb_tracks":2,"tracks":{"data":[{"id":1,"title":"First"}],"next":"https://untrusted.test"}}`
		case "/album/42/tracks":
			if r.URL.Query().Get("index") != "1" {
				t.Fatal("wrong page")
			}
			body = `{"data":[{"id":2,"title":"Second"}]}`
		case "/search/album":
			body = `{"data":[{"id":42,"title":"Oxymoron (Deluxe)","artist":{"name":"Schoolboy Q"}},{"id":43,"title":"Oxymoron","artist":{"name":"Schoolboy Q"}}]}`
		case "/album/44":
			body = `{"id":44,"title":"Broken","artist":{"name":"Artist"},"nb_tracks":2,"tracks":{"data":[{"id":1,"title":"First"},{"id":1,"title":"Duplicate"}]}}`
		default:
			t.Fatalf("unexpected endpoint: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	r := httptest.NewRequest("GET", "/api/v1/acquisition/jobs/preview/42", nil)
	r.SetPathValue("albumId", "42")
	w := httptest.NewRecorder()
	a.previewAlbum(w, r)
	if w.Code != 200 {
		t.Fatalf("preview failed: %s", w.Body)
	}
	var preview deezerAlbum
	if json.Unmarshal(w.Body.Bytes(), &preview) != nil || len(preview.Tracks.Data) != 2 {
		t.Fatal("preview must include all pages")
	}
	job := &acquisitionJob{ID: strings.Repeat("a", 32), Artist: "Schoolboy Q", Title: "Oxymoron (Deluxe)", Kind: "album", CatalogueAlbumID: "42"}
	count, err := a.jobTrackCount(context.Background(), job)
	if err != nil || count != 2 || requests != 2 {
		t.Fatalf("cached acquisition: count=%d requests=%d err=%v", count, requests, err)
	}
	job.CatalogueAlbumID, job.ExpectedTracks = "", 0 // Existing failed history has no catalogue ID.
	count, err = a.jobTrackCount(context.Background(), job)
	if err != nil || count != 2 || requests != 3 || job.CatalogueAlbumID != "42" {
		t.Fatal("old job must select exact deluxe edition and reuse cache")
	}
	job.Title, job.ExpectedTracks = "Oxymoron", 0
	if _, err = a.jobTrackCount(context.Background(), job); err == nil {
		t.Fatal("must reject wrong edition")
	}
	if _, err = a.catalogueAlbum(context.Background(), "44"); err == nil {
		t.Fatal("must reject duplicate tracks")
	}
	if _, err = a.catalogueAlbum(context.Background(), "../42"); err == nil {
		t.Fatal("must validate IDs")
	}
}

func TestRetryFailedDownloadAndGuardUncertainTransfers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &acquisitionService{ctx: ctx, client: &http.Client{}, cfg: config{downloadPath: t.TempDir(), slskdURL: "http://slskd.test", providerOrder: []string{"slskd"}}, jobs: make(map[string]*acquisitionJob)}
	id := strings.Repeat("a", 32)
	a.jobs[id] = &acquisitionJob{ID: id, Artist: "Tyler, The Creator", Title: "OKRA", Kind: "album", Status: "failed", Error: "release_not_found"}
	retry := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.create(w, httptest.NewRequest("POST", "/api/v1/acquisition/jobs", strings.NewReader(`{"retry_id":"`+id+`"}`)))
		return w
	}
	result := retry()
	var job acquisitionJob
	if result.Code != 202 || json.Unmarshal(result.Body.Bytes(), &job) != nil || job.ID == id || job.Artist != "Tyler, The Creator" || job.Title != "OKRA" {
		t.Fatalf("retry: %d %s", result.Code, result.Body)
	}
	a.mu.Lock()
	a.jobs[id].Error = "slskd_enqueue_failed"
	a.mu.Unlock()
	if result = retry(); result.Code != 409 {
		t.Fatal("uncertain enqueue must not duplicate a transfer")
	}
	a.mu.Lock()
	a.jobs[id].Error, a.jobs[id].BatchID, a.jobs[id].Source = "upstream_http_503", "original-batch", "slskd"
	a.mu.Unlock()
	result = retry()
	if result.Code != 202 || json.Unmarshal(result.Body.Bytes(), &job) != nil || job.ID != id || job.BatchID != "original-batch" || job.Status != "downloading" {
		t.Fatalf("connection failure must resume original batch: %d %s", result.Code, result.Body)
	}
}

type catalogueTestTransport func(*http.Request) (*http.Response, error)

func (f catalogueTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
