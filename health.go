package main

import (
	"bytes"
	"context"
	"crypto/md5" // Subsonic's token protocol requires MD5(password + fresh salt), not password hashing.
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

type dependencyHealth struct {
	Reachable     bool   `json:"reachable"`
	Configured    bool   `json:"configured"`
	Authenticated bool   `json:"authenticated"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	Error         string `json:"error,omitempty"`
}

type healthReport struct {
	Service           string                      `json:"service"`
	Status            string                      `json:"status"`
	UptimeSeconds     int64                       `json:"uptime_seconds"`
	CheckedAt         string                      `json:"checked_at"`
	LibraryAccessible bool                        `json:"library_accessible"`
	Dependencies      map[string]dependencyHealth `json:"dependencies"`
	DownloadsEnabled  bool                        `json:"downloads_enabled"`
	LibraryWritable   bool                        `json:"library_writable"`
	StateWritable     bool                        `json:"state_writable"`
	DownloadsWritable bool                        `json:"downloads_writable"`
}

func healthHandler(c config, started time.Time) http.HandlerFunc {
	client := &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		navCh, slskCh := make(chan dependencyHealth, 1), make(chan dependencyHealth, 1)
		go func() { navCh <- checkNavidrome(ctx, client, c) }()
		go func() {
			if c.downloadPath == "" || c.slskdURL == "" {
				slskCh <- dependencyHealth{}
				return
			}
			slskCh <- checkSlskd(ctx, client, c)
		}()
		nav, slsk := <-navCh, <-slskCh
		library, err := os.Open(c.libraryPath)
		accessible := false
		if err == nil {
			// Read one directory entry to verify traversal permission without listing filenames.
			_, err = library.Readdirnames(1)
			accessible = err == nil || errors.Is(err, io.EOF)
			_ = library.Close()
		}
		status, httpStatus := "ok", http.StatusOK
		dependencies := map[string]dependencyHealth{"navidrome": nav}
		if c.downloadPath != "" && c.slskdURL != "" {
			dependencies["slskd"] = slsk
		}
		libraryWritable, downloadsWritable := false, false
		if c.downloadPath != "" {
			libraryWritable, downloadsWritable = writableDirectory(c.libraryPath), writableDirectory(c.downloadPath)
		}
		stateWritable := c.statePath != "" && writableDirectory(c.statePath)
		if !nav.Authenticated || (c.statePath != "" && !stateWritable) || (c.downloadPath != "" && (!accessible || !libraryWritable || !downloadsWritable || c.slskdURL != "" && !slsk.Authenticated)) {
			status, httpStatus = "degraded", http.StatusServiceUnavailable
		}
		writeJSON(w, httpStatus, healthReport{Service: "bitchord-plus-companion", Status: status,
			UptimeSeconds: int64(time.Since(started).Seconds()), CheckedAt: time.Now().UTC().Format(time.RFC3339),
			LibraryAccessible: accessible,
			StateWritable:     stateWritable, Dependencies: dependencies, DownloadsEnabled: c.downloadPath != "", LibraryWritable: libraryWritable, DownloadsWritable: downloadsWritable})
	}
}

func writableDirectory(directory string) bool {
	file, err := os.CreateTemp(directory, ".glaze-health-*")
	if err != nil {
		return false
	}
	name := file.Name()
	_, writeErr := file.Write([]byte("Glaze permission check"))
	closeErr := file.Close()
	removeErr := os.Remove(name)
	return writeErr == nil && closeErr == nil && removeErr == nil
}

// Error messages deliberately omit URLs, response bodies, and authentication material.
func fetch(ctx context.Context, client *http.Client, method, endpoint string, headers http.Header, body []byte) (dependencyHealth, []byte) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return dependencyHealth{Error: "invalid_request"}, nil
	}
	req.Header = headers
	response, err := client.Do(req)
	result := dependencyHealth{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		result.Error = "connection_failed"
		if ctx.Err() != nil {
			result.Error = "timeout_or_cancelled"
		}
		return result, nil
	}
	defer response.Body.Close()
	result.Reachable, result.HTTPStatus = true, response.StatusCode
	data, err := io.ReadAll(io.LimitReader(response.Body, 16*1024+1))
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil || len(data) > 16*1024 {
		result.Error = "invalid_response"
		return result, nil
	}
	if response.StatusCode != http.StatusOK {
		result.Error = "api_rejected"
		if response.StatusCode == 401 || response.StatusCode == 403 {
			result.Error = "authentication_failed"
		}
	}
	return result, data
}

func checkNavidrome(ctx context.Context, client *http.Client, c config) dependencyHealth {
	configured := c.navidromeUsername != "" && c.navidromePassword != ""
	params := url.Values{"v": {"1.16.1"}, "c": {"BitChordPlusCompanion"}, "f": {"json"}}
	if configured {
		saltBytes := make([]byte, 16)
		if _, err := rand.Read(saltBytes); err != nil {
			return dependencyHealth{Configured: true, Error: "salt_generation_failed"}
		}
		salt := hex.EncodeToString(saltBytes)
		token := md5.Sum([]byte(c.navidromePassword + salt))
		params.Set("u", c.navidromeUsername)
		params.Set("s", salt)
		params.Set("t", hex.EncodeToString(token[:]))
	}
	result, data := fetch(ctx, client, http.MethodGet, c.navidromeURL+"/rest/ping.view?"+params.Encode(), nil, nil)
	result.Configured = configured
	if result.Error != "" {
		return result
	}
	var payload struct {
		Response struct {
			Status string `json:"status"`
			Error  struct {
				Code int `json:"code"`
			} `json:"error"`
		} `json:"subsonic-response"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Response.Status == "" {
		result.Error = "invalid_response"
	} else if !configured {
		result.Error = "credentials_not_configured"
	} else if payload.Response.Status == "ok" {
		result.Authenticated = true
	} else if payload.Response.Error.Code == 40 || payload.Response.Error.Code == 41 {
		result.Error = "authentication_failed"
	} else {
		result.Error = "api_rejected"
	}
	return result
}

func checkSlskd(ctx context.Context, client *http.Client, c config) dependencyHealth {
	start := time.Now()
	headers := http.Header{}
	configured := c.slskdAPIKey != "" || (c.slskdUsername != "" && c.slskdPassword != "")
	if c.slskdAPIKey != "" {
		headers.Set("X-API-Key", c.slskdAPIKey)
	} else if configured {
		credentials, _ := json.Marshal(map[string]string{"username": c.slskdUsername, "password": c.slskdPassword})
		result, body := fetch(ctx, client, http.MethodPost, c.slskdURL+"/api/v0/session",
			http.Header{"Content-Type": {"application/json"}}, credentials)
		result.Configured = true
		if result.Error != "" {
			return result
		}
		var payload struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Token == "" {
			result.Error = "invalid_response"
			return result
		}
		headers.Set("Authorization", "Bearer "+payload.Token)
	}
	result, _ := fetch(ctx, client, http.MethodGet, c.slskdURL+"/api/v0/session", headers, nil)
	result.Configured, result.LatencyMS = configured, time.Since(start).Milliseconds()
	if !configured && result.Reachable {
		result.Error = "credentials_not_configured"
	}
	result.Authenticated = configured && result.HTTPStatus == http.StatusOK && result.Error == ""
	return result
}
