package main

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// setCover records [cover] (an https image URL) as the job's artwork, if it has none yet.
func (a *acquisitionService) setCover(id, cover string) {
	if u, err := url.Parse(cover); err != nil || u.Scheme != "https" || u.User != nil || len(cover) > 1024 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if j := a.jobs[id]; j != nil && j.Cover == "" {
		snapshot := *j
		snapshot.Cover, snapshot.keepTime = cover, true
		_ = a.updateLocked(&snapshot)
	}
}

// fillCovers gives jobs without artwork (older ones, and requests sent without it) the cover
// Deezer has for them, newest first, a few a second. It keeps watching for new ones.
func (a *acquisitionService) fillCovers() {
	tried := map[string]bool{}
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
		a.mu.Lock()
		var missing []acquisitionJob
		for _, j := range a.jobs {
			if j.Cover == "" && !j.Hidden && !tried[j.ID] {
				missing = append(missing, *j)
			}
		}
		a.mu.Unlock()
		sort.Slice(missing, func(i, k int) bool { return missing[i].UpdatedAt.After(missing[k].UpdatedAt) })
		found := map[string]string{}
		for _, j := range missing {
			tried[j.ID] = true
			key := strings.ToLower(j.Artist + "|" + j.Title + "|" + j.Kind)
			cover, seen := found[key]
			if !seen {
				cover = a.deezerCover(a.ctx, j)
				found[key] = cover
				time.Sleep(250 * time.Millisecond)
			}
			if cover != "" {
				a.setCover(j.ID, cover)
			}
			if a.ctx.Err() != nil {
				return
			}
		}
	}
}

// deezerCover: the album artwork Deezer has for a job's album, or for the album its song is on.
func (a *acquisitionService) deezerCover(ctx context.Context, j acquisitionJob) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	q := cleanArtist(j.Artist) + " " + j.Title
	if j.Kind == "album" {
		var found struct {
			Data []struct {
				Title  string                `json:"title"`
				Cover  string                `json:"cover_medium"`
				Artist struct{ Name string } `json:"artist"`
			} `json:"data"`
		}
		if _, err := a.request(ctx, http.MethodGet, "https://api.deezer.com/search/album?limit=10&q="+url.QueryEscape(q), nil, nil, &found); err != nil {
			return ""
		}
		for _, al := range found.Data {
			if baseTitle(al.Title) == baseTitle(j.Title) && overlap(credits(j.Artist), credits(al.Artist.Name)) {
				return al.Cover
			}
		}
		return ""
	}
	var found struct {
		Data []struct {
			Title  string                `json:"title"`
			Artist struct{ Name string } `json:"artist"`
			Album  struct {
				Cover string `json:"cover_medium"`
			} `json:"album"`
		} `json:"data"`
	}
	if _, err := a.request(ctx, http.MethodGet, "https://api.deezer.com/search/track?limit=10&q="+url.QueryEscape(q), nil, nil, &found); err != nil {
		return ""
	}
	for _, t := range found.Data {
		if baseTitle(t.Title) == baseTitle(j.Title) && overlap(credits(j.Artist), credits(t.Artist.Name)) {
			return t.Album.Cover
		}
	}
	return ""
}
