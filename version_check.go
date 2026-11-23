package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// latestRelease asks GitHub for the newest companion release at most every six hours,
// so the app can say when this companion is behind. It never updates anything itself.
var latestRelease = &releaseCheck{url: "https://api.github.com/repos/ILostXD/BitChord_Plus_Companion/releases/latest"}

type releaseCheck struct {
	url     string
	mu      sync.Mutex
	tag     string
	checked time.Time
}

func (c *releaseCheck) latest(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) < 6*time.Hour {
		return c.tag
	}
	c.checked = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "BitChordPlusCompanion/"+serviceVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return c.tag
	}
	defer resp.Body.Close()
	var body struct {
		Tag string `json:"tag_name"`
	}
	if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil && body.Tag != "" {
		c.tag = strings.TrimPrefix(body.Tag, "v")
	}
	return c.tag
}

// newerVersion reports whether a is a later release than b, comparing the numeric
// parts of "1.2.3"; a pre-release ("1.0.0-alpha.9") is older than its release.
func newerVersion(a, b string) bool {
	core := func(v string) ([]int, bool) {
		main, pre, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
		var parts []int
		for _, p := range strings.Split(main, ".") {
			n, _ := strconv.Atoi(p)
			parts = append(parts, n)
		}
		return parts, pre != ""
	}
	pa, preA := core(a)
	pb, preB := core(b)
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return preB && !preA
}

func versionInfo(ctx context.Context) map[string]any {
	latest := latestRelease.latest(ctx)
	return map[string]any{"version": serviceVersion, "latest_version": latest,
		"update_available": latest != "" && newerVersion(latest, serviceVersion)}
}
