package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/ping.view":
			query := r.URL.Query()
			if query.Get("p") != "" || query.Get("s") == "" {
				t.Error("use salted tokens, never plaintext passwords")
			}
			token := md5.Sum([]byte("nav-secret" + query.Get("s")))
			if query.Get("t") != hex.EncodeToString(token[:]) || query.Get("u") != "nav-user" {
				writeJSON(w, 200, map[string]any{"subsonic-response": map[string]any{"status": "failed", "error": map[string]int{"code": 40}}})
				return
			}
			writeJSON(w, 200, map[string]any{"subsonic-response": map[string]string{"status": "ok"}})
		case "/api/v0/session":
			if r.Method == "POST" {
				var login map[string]string
				_ = json.NewDecoder(r.Body).Decode(&login)
				if login["username"] != "slsk-user" || login["password"] != "slsk-secret" {
					w.WriteHeader(401)
					return
				}
				writeJSON(w, 200, map[string]string{"token": "session-token"})
			} else if r.Header.Get("X-API-Key") == "api-key" || r.Header.Get("Authorization") == "Bearer session-token" {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(401)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	cfg := config{navidromeURL: upstream.URL, navidromeUsername: "nav-user", navidromePassword: "nav-secret",
		slskdURL: upstream.URL, slskdAPIKey: "api-key", libraryPath: t.TempDir(), downloadPath: t.TempDir()}
	for _, scenario := range []string{"healthy-key", "healthy-login", "wrong-password", "unreachable", "missing-credentials"} {
		t.Run(scenario, func(t *testing.T) {
			c := cfg
			if scenario == "healthy-login" {
				c.slskdAPIKey = ""
				c.slskdUsername = "slsk-user"
				c.slskdPassword = "slsk-secret"
			}
			if scenario == "wrong-password" {
				c.navidromePassword = "wrong"
			}
			if scenario == "unreachable" {
				c.navidromeURL = "http://127.0.0.1:1"
			}
			if scenario == "missing-credentials" {
				c.slskdAPIKey = ""
			}
			result := httptest.NewRecorder()
			healthHandler(c, time.Now().Add(-2*time.Second))(result, httptest.NewRequest("GET", "/health", nil))
			var report healthReport
			if json.Unmarshal(result.Body.Bytes(), &report) != nil {
				t.Fatal("not JSON")
			}
			if report.UptimeSeconds < 2 || !report.LibraryAccessible {
				t.Fatal("missing uptime/library check")
			}
			want := 503
			if strings.HasPrefix(scenario, "healthy") {
				want = 200
			}
			if result.Code != want {
				t.Fatalf("got %d: %s", result.Code, result.Body.String())
			}
			nav := report.Dependencies["navidrome"]
			if scenario == "wrong-password" && (!nav.Reachable || nav.Authenticated || nav.Error != "authentication_failed") {
				t.Fatal("HTTP 200 with failed Subsonic status must not count as authenticated")
			}
			if scenario == "unreachable" && nav.Reachable {
				t.Fatal("connection failure must be unreachable")
			}
			if strings.Contains(result.Body.String(), "secret") || strings.Contains(result.Body.String(), "token=") {
				t.Fatal("health leaked authentication data")
			}
		})
	}
}
