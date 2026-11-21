package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const serviceVersion = "1.0.0-alpha.9"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	token := os.Getenv("COMPANION_API_TOKEN")
	if len(token) < 32 || strings.HasPrefix(token, "replace-") {
		slog.Error("COMPANION_API_TOKEN must be a random token of at least 32 characters")
		os.Exit(1)
	}
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration_invalid", "error", err.Error())
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: ":8080", Handler: newHandler(token, ctx, cfg),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 * 1024}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		slog.Info("shutdown_started")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown_failed")
		}
	}()
	slog.Info("server_started", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server_failed", "error", err.Error())
		os.Exit(1)
	}
	<-shutdownDone
	slog.Info("server_stopped")
}

func newHandler(token string, ctx context.Context, cfg config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"service": "bitchord-plus-companion", "version": serviceVersion})
	})
	mux.HandleFunc("/api/v1/tagging/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusGone, map[string]string{"error": "smart_tagging_removed"})
	})
	registerREST(mux)
	registerLastfmMetadata(mux, cfg)
	acquisition := registerAcquisition(mux, ctx, cfg)
	registerReleases(mux, ctx, acquisition)
	jam := &jamHub{sessions: make(map[string]*jamSession), cfg: cfg}
	jamAuthClient := &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	jam.registerREST(mux)
	jam.registerLibrary(mux, ctx)
	jam.registerContributions(mux, ctx)
	mux.HandleFunc("GET /api/v1/jam/ws", jam.socket(ctx))
	mux.HandleFunc("GET /dashboard", serveDashboard)
	mux.HandleFunc("GET /health", healthHandler(cfg, time.Now()))
	mux.HandleFunc("GET /api/v1/storage", storageHandler(cfg))
	mux.HandleFunc("GET /api/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"jam": true, "jam_streaming": true, "downloads": cfg.downloadPath != "", "fingerprinting": cfg.acoustidKey != "", "lastfm_metadata": cfg.lastfmKey != ""})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		w = rec
		// Log paths only: never headers, query strings, request bodies, or credentials.
		defer func() {
			slog.Info("request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Jam invitations and member tokens authorize only Jam routes; never grant
		// access to the administrative acquisition API.
		jamMemberRoute := strings.HasPrefix(r.URL.Path, "/api/v1/jam/sessions/") ||
			(r.URL.Path == "/api/v1/jam/ws" && r.Method == http.MethodGet)
		admin := subtle.ConstantTimeCompare(
			[]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
		jamCreate := r.Method == http.MethodPost && r.URL.Path == "/api/v1/jam/sessions"
		acquisitionRoute := (r.Method == http.MethodGet || r.Method == http.MethodPost) && strings.HasPrefix(r.URL.Path, "/api/v1/acquisition/jobs")
		releaseRoute := (r.Method == http.MethodGet || r.Method == http.MethodPut || r.Method == http.MethodPost) && (r.URL.Path == "/api/v1/releases" || strings.HasPrefix(r.URL.Path, "/api/v1/releases/"))
		storageRoute := r.Method == http.MethodGet && (r.URL.Path == "/api/v1/storage" || r.URL.Path == "/api/v1/capabilities" || r.URL.Path == "/api/v1/metadata/artist" || r.URL.Path == "/api/v1/metadata/album")
		taggingRoute := (r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete) &&
			(r.URL.Path == "/api/v1/tagging/jobs" || strings.HasPrefix(r.URL.Path, "/api/v1/tagging/jobs/"))
		companionUser := false
		hostAuthenticated := jamCreate && authenticateJamHost(r, cfg, jamAuthClient)
		if hostAuthenticated {
			identity, _ := navidromeRequestIdentity(r, cfg)
			r = r.WithContext(context.WithValue(r.Context(), navidromeIdentityKey{}, identity))
		}
		if !admin && (acquisitionRoute || releaseRoute || storageRoute || taggingRoute) {
			identity, valid := authenticateCompanionUser(r, cfg, jamAuthClient)
			companionUser = valid && (!taggingRoute || strings.EqualFold(identity.User, cfg.navidromeUsername))
			if companionUser {
				r = r.WithContext(context.WithValue(r.Context(), navidromeIdentityKey{}, identity))
			}
		}
		if r.URL.Path != "/live" && r.URL.Path != "/dashboard" && !jamMemberRoute && !admin &&
			!hostAuthenticated && !companionUser {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// statusRecorder lets the request log say how each request ended.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int)        { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack_unsupported")
	}
	return h.Hijack()
}
