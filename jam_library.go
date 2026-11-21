package main

import (
	"context"
	"crypto/md5" // Compatibility with Subsonic; the member secret is random 256-bit entropy.
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Expose a small read-only Subsonic surface using the session member's secret.
// The upstream is always the configured Navidrome, authenticated as the host.
func (h *jamHub) registerLibrary(mux *http.ServeMux, root context.Context) {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	mux.HandleFunc("GET /api/v1/jam/sessions/{sessionId}/library/rest/{endpoint}", func(w http.ResponseWriter, r *http.Request) {
		endpoint := strings.TrimSuffix(r.PathValue("endpoint"), ".view")
		if !slices.Contains([]string{"ping", "getSong", "search3", "getCoverArt", "stream", "getLyricsBySongId"}, endpoint) {
			jamError(w, 403, "jam_read_only")
			return
		}
		query := r.URL.Query()
		salt, token := query.Get("s"), query.Get("t")
		if len(salt) < 16 || len(salt) > 128 || len(token) != 32 || !jamHex.MatchString(salt) {
			jamError(w, 403, "invalid_member_token")
			return
		}
		h.mu.Lock()
		h.expireLocked()
		session := h.sessions[r.PathValue("sessionId")]
		if session == nil {
			h.mu.Unlock()
			jamError(w, 404, "session_not_found")
			return
		}
		member := session.Members[query.Get("u")]
		if member == nil {
			h.mu.Unlock()
			jamError(w, 403, "invalid_member_token")
			return
		}
		sum := md5.Sum([]byte(member.token + salt))
		if subtle.ConstantTimeCompare([]byte(token), []byte(hex.EncodeToString(sum[:]))) != 1 {
			h.mu.Unlock()
			jamError(w, 403, "invalid_member_token")
			return
		}
		if endpoint == "stream" {
			if member.streams >= 3 {
				h.mu.Unlock()
				jamError(w, 429, "jam_stream_limit")
				return
			}
			member.streams++
			defer func() { h.mu.Lock(); member.streams--; h.mu.Unlock() }()
		}
		identity := session.libraryIdentity
		contribution := session.contributions[query.Get("id")]
		id := query.Get("id")
		inQueue := id == session.Playback.TrackID || slices.ContainsFunc(session.Queue, func(item jamQueueItem) bool { return item.TrackID == id })
		allowed := endpoint == "ping" || endpoint == "search3" ||
			(validTrackID(id) && ((endpoint == "getCoverArt") ||
				(endpoint != "getCoverArt" && (inQueue || endpoint == "getSong" && session.searchTracks[id]))))
		h.mu.Unlock()
		if !allowed {
			jamError(w, 403, "track_not_in_session")
			return
		}
		if endpoint == "stream" {
			ctx, stop := context.WithTimeout(r.Context(), 2*time.Hour)
			defer stop()
			r = r.WithContext(ctx)
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-root.Done():
						stop()
						return
					case <-ticker.C:
						h.mu.Lock()
						active := h.sessions[session.ID] == session && session.Members[member.ID] == member && time.Now().Before(session.Expires)
						if contribution != nil {
							active = active && session.contributions[id] == contribution
						}
						h.mu.Unlock()
						if !active {
							stop()
							return
						}
					}
				}
			}()
			w = jamContextWriter{w, ctx}
		}
		if contribution != nil {
			switch endpoint {
			case "getSong":
				writeJSON(w, 200, map[string]any{"subsonic-response": map[string]any{"status": "ok", "song": contribution.song}})
			case "getCoverArt":
				w.Header().Set("Content-Type", http.DetectContentType(contribution.cover))
				_, _ = w.Write(contribution.cover)
			case "stream":
				file, err := os.Open(contribution.filename)
				if err != nil {
					jamError(w, 410, "song_no_longer_available")
					return
				}
				defer file.Close()
				w.Header().Set("Content-Type", contribution.contentType)
				// ServeContent preserves byte ranges and seeking without loading audio into memory.
				http.ServeContent(w, r, "audio", time.Time{}, file)
			case "getLyricsBySongId":
				writeJSON(w, 200, map[string]any{"subsonic-response": map[string]any{"status": "ok", "lyricsList": map[string]any{"structuredLyrics": []any{}}}})
			default:
				jamError(w, 403, "jam_read_only")
			}
			return
		}
		if identity.User == "" {
			jamError(w, 503, "jam_library_unavailable")
			return
		}
		upstream, _ := url.Parse(identity.endpoint(h.cfg, endpoint))
		params := upstream.Query()
		switch endpoint {
		case "getSong", "stream", "getCoverArt", "getLyricsBySongId":
			params.Set("id", id)
		}
		if endpoint == "stream" {
			params.Set("format", "raw")
		}
		if endpoint == "getCoverArt" {
			size, _ := strconv.Atoi(query.Get("size"))
			params.Set("size", strconv.Itoa(max(32, min(size, 1024))))
		}
		if endpoint == "search3" {
			text := strings.TrimSpace(query.Get("query"))
			if len(text) == 0 || len(text) > 200 {
				jamError(w, 400, "invalid_search")
				return
			}
			params.Set("query", text)
			params.Set("artistCount", "0")
			params.Set("albumCount", "0")
			params.Set("songCount", "50")
		}
		upstream.RawQuery = params.Encode()
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		if endpoint == "stream" {
			cancel()
			ctx, cancel = context.WithTimeout(r.Context(), 2*time.Hour)
		}
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.String(), nil)
		if err != nil {
			jamError(w, 502, "jam_library_unavailable")
			return
		}
		if endpoint == "stream" {
			request.Header.Set("Range", r.Header.Get("Range"))
			request.Header.Set("If-Range", r.Header.Get("If-Range"))
		}
		response, err := client.Do(request)
		if err != nil {
			jamError(w, 502, "jam_library_unavailable")
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 206 && response.StatusCode != 416 {
			jamError(w, 502, "jam_library_unavailable")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		if endpoint == "stream" || endpoint == "getCoverArt" {
			if endpoint == "getCoverArt" && !strings.HasPrefix(response.Header.Get("Content-Type"), "image/") {
				jamError(w, 502, "jam_artwork_unavailable")
				return
			}
			if endpoint == "stream" && !strings.HasPrefix(response.Header.Get("Content-Type"), "audio/") &&
				!strings.HasPrefix(response.Header.Get("Content-Type"), "application/octet-stream") && response.StatusCode != 416 {
				jamError(w, 502, "jam_audio_unavailable")
				return
			}
			for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
				if value := response.Header.Get(name); value != "" {
					w.Header().Set(name, value)
				}
			}
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
			return
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		var body struct {
			Subsonic struct {
				Status string          `json:"status"`
				Song   json.RawMessage `json:"song"`
				Search struct {
					Songs []json.RawMessage `json:"song"`
				} `json:"searchResult3"`
			} `json:"subsonic-response"`
		}
		if err != nil || json.Unmarshal(data, &body) != nil || body.Subsonic.Status != "ok" {
			jamError(w, 502, "jam_library_unavailable")
			return
		}
		h.mu.Lock()
		if session.artwork == nil {
			session.artwork = make(map[string]bool)
		}
		if session.searchTracks == nil {
			session.searchTracks = make(map[string]bool)
		}
		if len(session.searchTracks) > 2048 {
			clear(session.searchTracks)
		}
		if len(session.artwork) > 4096 {
			clear(session.artwork)
		}
		tracks := body.Subsonic.Search.Songs
		if len(body.Subsonic.Song) > 0 {
			tracks = append(tracks, body.Subsonic.Song)
		}
		for _, raw := range tracks {
			var song struct {
				ID      string `json:"id"`
				Cover   string `json:"coverArt"`
				AlbumID string `json:"albumId"`
			}
			if json.Unmarshal(raw, &song) == nil {
				session.searchTracks[song.ID] = true
				if song.Cover != "" {
					session.artwork[song.Cover] = true
					if song.AlbumID != "" && (strings.HasPrefix(song.Cover, "mf-") || strings.HasPrefix(song.Cover, "dc-")) {
						session.artwork["al-"+song.AlbumID] = true
					}
				}
			}
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var safe map[string]any
		if json.Unmarshal(data, &safe) != nil {
			jamError(w, 502, "jam_library_unavailable")
			return
		}
		sub, _ := safe["subsonic-response"].(map[string]any)
		if song, ok := sub["song"].(map[string]any); ok {
			cleanJamMetadata(song)
		}
		if result, ok := sub["searchResult3"].(map[string]any); ok {
			delete(result, "artist")
			delete(result, "album")
			if tracks, ok := result["song"].([]any); ok {
				for _, raw := range tracks {
					if song, ok := raw.(map[string]any); ok {
						cleanJamMetadata(song)
					}
				}
			}
		}
		writeJSON(w, 200, safe)
	})
}

func cleanJamMetadata(song map[string]any) {
	for key := range song {
		if !slices.Contains([]string{"id", "title", "artist", "album", "artistId", "albumId", "coverArt", "duration", "track", "discNumber", "genre", "suffix", "samplingRate", "bitRate", "isExplicit", "artists"}, key) {
			delete(song, key)
		}
	}
}

type jamContextWriter struct {
	http.ResponseWriter
	ctx context.Context
}

func (w jamContextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(data)
}
