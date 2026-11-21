package main

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type lastfmMetadata struct {
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	SourceURL   string   `json:"source_url"`
}

type lastfmCacheEntry struct {
	data    lastfmMetadata
	fetched time.Time
}

var lastfmHTML = regexp.MustCompile(`<[^>]*>`)

func lastfmPlain(value string) string {
	value = strings.Join(strings.Fields(html.UnescapeString(lastfmHTML.ReplaceAllString(value, " "))), " ")
	text := []rune(value)
	if len(text) > 4000 {
		text = text[:4000]
	}
	return string(text)
}

func registerLastfmMetadata(mux *http.ServeMux, cfg config) {
	client := &http.Client{Timeout: 6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// ponytail: serialize optional cache misses; use per-key locks if enrichment traffic grows.
	var lock sync.Mutex
	cache := make(map[string]lastfmCacheEntry)
	handler := func(w http.ResponseWriter, r *http.Request) {
		if cfg.lastfmKey == "" {
			writeJSON(w, http.StatusOK, map[string]bool{"enabled": false})
			return
		}
		kind := strings.TrimPrefix(r.URL.Path, "/api/v1/metadata/")
		artist, album := strings.TrimSpace(r.URL.Query().Get("artist")), strings.TrimSpace(r.URL.Query().Get("album"))
		if artist == "" || len(artist) > 512 || len(album) > 512 || strings.ContainsAny(artist+album, "\r\n\x00") || (kind == "album" && album == "") {
			writeJSON(w, 400, map[string]string{"error": "invalid_metadata_query"})
			return
		}
		key := kind + "\x00" + strings.ToLower(artist) + "\x00" + strings.ToLower(album)
		lock.Lock()
		defer lock.Unlock()
		if hit, ok := cache[key]; ok && time.Since(hit.fetched) < 24*time.Hour {
			writeJSON(w, 200, hit.data)
			return
		}
		params := url.Values{"method": {kind + ".getInfo"}, "api_key": {cfg.lastfmKey}, "artist": {artist}, "autocorrect": {"0"}, "format": {"json"}}
		if kind == "album" {
			params.Set("album", album)
		}
		// Use the documented read endpoint; never log this URL or upstream errors (the key is in its query).
		request, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://ws.audioscrobbler.com/2.0/?"+params.Encode(), nil)
		response, err := client.Do(request)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": "metadata_unavailable"})
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		var envelope map[string]json.RawMessage
		if err != nil || len(body) > 1<<20 || response.StatusCode != 200 || json.Unmarshal(body, &envelope) != nil || envelope["error"] != nil {
			writeJSON(w, 502, map[string]string{"error": "metadata_unavailable"})
			return
		}
		data, matched := parseLastfmMetadata(envelope[kind], kind, artist, album)
		if !matched {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if len(cache) >= 256 {
			var oldest string
			for k, v := range cache {
				if oldest == "" || v.fetched.Before(cache[oldest].fetched) {
					oldest = k
				}
			}
			delete(cache, oldest)
		}
		cache[key] = lastfmCacheEntry{data, time.Now()}
		writeJSON(w, 200, data)
	}
	mux.HandleFunc("GET /api/v1/metadata/artist", handler)
	mux.HandleFunc("GET /api/v1/metadata/album", handler)
}

// Exact names only. Last.fm name corrections cannot establish artist or release identity.
func parseLastfmMetadata(body []byte, kind, artist, album string) (lastfmMetadata, bool) {
	var item struct {
		Name, Artist, URL string
		Bio, Wiki         struct{ Summary string }
		Tags, TopTags     struct{ Tag []struct{ Name string } }
	}
	if json.Unmarshal(body, &item) != nil {
		return lastfmMetadata{}, false
	}
	if kind == "artist" && !strings.EqualFold(strings.TrimSpace(item.Name), artist) ||
		kind == "album" && (!strings.EqualFold(strings.TrimSpace(item.Artist), artist) || !strings.EqualFold(strings.TrimSpace(item.Name), album)) {
		return lastfmMetadata{}, false
	}
	source, err := url.Parse(item.URL)
	if err != nil || source.Scheme != "https" || source.User != nil || source.RawQuery != "" ||
		(source.Host != "www.last.fm" && source.Host != "last.fm") || !strings.HasPrefix(source.Path, "/music/") {
		return lastfmMetadata{}, false
	}
	data := lastfmMetadata{Description: lastfmPlain(item.Bio.Summary), Tags: []string{}, SourceURL: source.String()}
	tags := item.Tags.Tag
	if kind == "album" {
		data.Description = lastfmPlain(item.Wiki.Summary)
		if len(tags) == 0 {
			tags = item.TopTags.Tag
		}
	}
	seen := make(map[string]bool)
	for _, tag := range tags {
		name := lastfmPlain(tag.Name)
		key := strings.ToLower(name)
		if name != "" && len(name) <= 80 && !seen[key] && len(data.Tags) < 8 {
			data.Tags = append(data.Tags, name)
			seen[key] = true
		}
	}
	return data, true
}
