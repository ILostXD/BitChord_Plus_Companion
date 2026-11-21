package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJamHostNavidromeAuth(t *testing.T) {
	nav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := "failed"
		if r.URL.Query().Get("u") == "andy" && r.URL.Query().Get("s") == strings.Repeat("a", 24) &&
			r.URL.Query().Get("t") == strings.Repeat("b", 32) {
			status = "ok"
		}
		_, _ = w.Write([]byte(`{"subsonic-response":{"status":"` + status + `"}}`))
	}))
	defer nav.Close()
	handler := newHandler(strings.Repeat("x", 32), context.Background(), config{navidromeURL: nav.URL})
	for _, tc := range []struct {
		user, token string
		want        int
	}{
		{"andy", strings.Repeat("b", 32), 201},
		{"andy", strings.Repeat("c", 32), 401},
		{"other", strings.Repeat("b", 32), 401},
	} {
		r := httptest.NewRequest("POST", "/api/v1/jam/sessions", strings.NewReader(`{"name":"Andy"}`))
		r.Header.Set("X-Jam-Navidrome-User", tc.user)
		r.Header.Set("X-Jam-Navidrome-Salt", strings.Repeat("a", 24))
		r.Header.Set("X-Jam-Navidrome-Token", tc.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.user, w.Code, tc.want)
		}
	}
}

func TestAcquisitionAndStorageAcceptNavidromeAccounts(t *testing.T) {
	nav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok","user":{"username":"` + r.URL.Query().Get("u") + `"}}}`))
	}))
	defer nav.Close()
	handler := newHandler(strings.Repeat("x", 32), context.Background(), config{
		navidromeURL: nav.URL, navidromeUsername: "andy", providerOrder: []string{"slskd", "qobuz"},
	})
	for _, tc := range []struct {
		user string
		want int
	}{{"andy", 200}, {"other", 200}} {
		r := httptest.NewRequest("GET", "/api/v1/acquisition/jobs", nil)
		r.Header.Set("X-Jam-Navidrome-User", tc.user)
		r.Header.Set("X-Jam-Navidrome-Salt", strings.Repeat("a", 24))
		r.Header.Set("X-Jam-Navidrome-Token", strings.Repeat("b", 32))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.user, w.Code, tc.want)
		}
	}
	for _, user := range []string{"andy", "other"} {
		r := httptest.NewRequest("GET", "/api/v1/storage", nil)
		r.Header.Set("X-Jam-Navidrome-User", user)
		r.Header.Set("X-Jam-Navidrome-Salt", strings.Repeat("a", 24))
		r.Header.Set("X-Jam-Navidrome-Token", strings.Repeat("b", 32))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := http.StatusServiceUnavailable // Authenticated, but no storage configured in this test.
		if w.Code != want {
			t.Fatalf("storage owner %s: got %d, want %d", user, w.Code, want)
		}
	}
}
