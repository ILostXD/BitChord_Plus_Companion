package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type lastfmTestTransport func(*http.Request) (*http.Response, error)

func (f lastfmTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLastfmOptionalCacheAndIdentity(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	calls := 0
	http.DefaultTransport = lastfmTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != "ws.audioscrobbler.com" || r.URL.Path != "/2.0/" || r.Method != "GET" {
			t.Fatal("unexpected metadata destination")
		}
		form := r.URL.Query()
		if form.Get("api_key") != "private-key" || form.Get("autocorrect") != "0" {
			t.Fatal("key/correction policy")
		}
		name := form.Get("artist")
		if name == "NAV" {
			name = "Nave"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"artist":{"name":"` + name + `","url":"https://www.last.fm/music/` + name + `","bio":{"summary":"<b>Biography</b> &amp; info"},"tags":{"tag":[{"name":"rap"},{"name":"Rap"}]}}}`))}, nil
	})
	for _, enabled := range []bool{false, true} {
		mux := http.NewServeMux()
		cfg := config{}
		if enabled {
			cfg.lastfmKey = "private-key"
		}
		registerLastfmMetadata(mux, cfg)
		for i := 0; i < 2; i++ {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/metadata/artist?artist=Cher", nil))
			if w.Code != 200 || strings.Contains(w.Body.String(), "private-key") {
				t.Fatal(w.Code, w.Body.String())
			}
			if enabled && (!strings.Contains(w.Body.String(), "Biography &amp;") && !strings.Contains(w.Body.String(), `Biography \u0026 info`)) {
				t.Fatal(w.Body.String())
			}
		}
		if !enabled && calls != 0 {
			t.Fatal("disabled feature contacted Last.fm")
		}
		if enabled {
			if calls != 1 {
				t.Fatal("metadata cache missed", calls)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/metadata/artist?artist=NAV", nil))
			if w.Code != 204 {
				t.Fatal("NAV accepted Nave", w.Code)
			}
		}
	}
	data, ok := parseLastfmMetadata([]byte(`{"name":"Album","artist":"Artist","url":"https://www.last.fm/music/Artist/Album","wiki":{"summary":"Album info"},"toptags":{"tag":[{"name":"soul"}]}}`), "album", "Artist", "Album")
	if !ok || data.Description != "Album info" || len(data.Tags) != 1 {
		t.Fatal(data, ok)
	}
	data, ok = parseLastfmMetadata([]byte(`{"name":"Album","artist":"Artist","url":"https://www.last.fm/music/Artist/Album","tags":{"tag":[{"name":"soul"}]}}`), "album", "Artist", "Album")
	if !ok || len(data.Tags) != 1 {
		t.Fatal("album tags", data, ok)
	}
	if _, ok = parseLastfmMetadata([]byte(`{"name":"Artist","url":"https://evil.test/music/Artist"}`), "artist", "Artist", ""); ok {
		t.Fatal("unsafe attribution URL")
	}
	// Even optional/disabled endpoints require normal Companion authorization.
	w := httptest.NewRecorder()
	newHandler(strings.Repeat("x", 32), context.Background(), config{}).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/metadata/artist?artist=Cher", nil))
	if w.Code != 401 {
		t.Fatal("anonymous metadata lookup authorized", w.Code)
	}
}
