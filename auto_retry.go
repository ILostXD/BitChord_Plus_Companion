package main

import (
	"strings"
	"time"
)

// Requests that failed only because nothing was found, or a peer dropped out, are asked for
// again by themselves: Soulseek's users come and go, so a copy missing now is often there later.
// Errors may carry detail after a colon ("catalogue_tracklist_unavailable: upstream_http_503").
// Every 6 hours for the first two days after the request, then daily, for 30 days.
var retryableErrors = map[string]bool{
	"no_suitable_slskd_match": true, "slskd_search_timeout": true, "slskd_search_failed": true,
	"slskd_download_stalled": true, "slskd_download_failed": true, "qobuz_no_matching_release": true,
	"catalogue_unavailable": true, "catalogue_tracklist_unavailable": true, "release_not_found": true, "fingerprint_service_unavailable": true,
}

const autoRetryFor = 30 * 24 * time.Hour

// nextAutoRetry is when a request first made at [first], whose newest attempt failed at [last],
// is tried again; zero once it has been tried for [autoRetryFor].
func nextAutoRetry(first, last time.Time) time.Time {
	interval := 24 * time.Hour
	if last.Sub(first) < 48*time.Hour {
		interval = 6 * time.Hour
	}
	next := last.Add(interval)
	if next.Sub(first) > autoRetryFor {
		return time.Time{}
	}
	return next
}

// retryPlan: for each request whose newest attempt failed in a way worth retrying, that
// attempt's job id and when it is tried again. Callers hold a.mu.
func (a *acquisitionService) retryPlan() map[string]time.Time {
	type request struct {
		first  time.Time
		newest *acquisitionJob
	}
	requests := map[string]*request{}
	for _, j := range a.jobs {
		if j.Hidden || j.Status == "cancelled" || j.CancelRequested {
			continue
		}
		artist := j.Artist
		if j.Kind == "album" {
			artist = a.albumArtistLocked(artist, j.Title) // one album asked for under two credits is one request
		}
		key := strings.ToLower(cleanArtist(artist) + "|" + j.Title + "|" + j.Kind + "|" + normalize(j.AlbumTitle))
		r := requests[key]
		if r == nil {
			requests[key] = &request{first: j.CreatedAt, newest: j}
			continue
		}
		if j.CreatedAt.Before(r.first) {
			r.first = j.CreatedAt
		}
		if j.CreatedAt.After(r.newest.CreatedAt) {
			r.newest = j
		}
	}
	plan := map[string]time.Time{}
	for _, r := range requests {
		if r.newest.Status != "failed" || !retryableErrors[strings.TrimSpace(strings.SplitN(r.newest.Error, ":", 2)[0])] {
			continue
		}
		if next := nextAutoRetry(r.first, r.newest.UpdatedAt); !next.IsZero() {
			plan[r.newest.ID] = next
		}
	}
	return plan
}

func (a *acquisitionService) autoRetryLoop() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.autoRetry(time.Now())
		}
	}
}

// autoRetry asks again for up to ten due requests, each first checked against the library.
func (a *acquisitionService) autoRetry(now time.Time) {
	a.mu.Lock()
	var due []acquisitionJob
	for id, at := range a.retryPlan() {
		if !at.After(now) && len(due) < 10 {
			due = append(due, *a.jobs[id])
		}
	}
	a.mu.Unlock()
	for _, j := range due {
		if a.ctx.Err() != nil {
			return
		}
		if a.coveredByLibrary(a.ctx, j) != nil {
			continue
		}
		job, err := a.enqueueCatalogueOpt(cleanArtist(j.Artist), j.Title, j.Kind, j.CatalogueAlbumID, j.AlbumTitle, j.AlbumArtist, true)
		if err == nil && j.Cover != "" {
			a.setCover(job.ID, j.Cover)
		}
		time.Sleep(time.Second)
	}
}

// askForAlbum: a song no one shares on its own is often there inside its album, so the whole
// album is asked for (unless the library has it, or it is the song's own single).
func (a *acquisitionService) askForAlbum(song acquisitionJob) {
	if song.Kind != "track" || song.AlbumTitle == "" || normalize(song.AlbumTitle) == normalize(song.Title) {
		return
	}
	artist := cleanArtist(song.AlbumArtist)
	if artist == "" {
		artist = cleanArtist(song.Artist)
	}
	if a.recordLibraryDecisionOpt(a.ctx, artist, song.AlbumTitle, "album", "", 0, false) != nil {
		return
	}
	if job, err := a.enqueueCatalogueOpt(artist, song.AlbumTitle, "album", "", "", "", false); err == nil && song.Cover != "" {
		a.setCover(job.ID, song.Cover)
	}
}

// albumArtistLocked: the artist one album is asked for under, so it is one request however it
// is credited. "Grand Theft Auto VI: The Album" asked for as Travis Scott's (a song's artist) and
// as Grand Theft Auto VI's is one album: the name in the title wins, else the first asked for
// with a shared credit. Callers hold a.mu.
func (a *acquisitionService) albumArtistLocked(artist, title string) string {
	named := func(who string) bool {
		n := normalize(cleanArtist(who))
		return n != "" && strings.Contains(normalize(title), n)
	}
	if named(artist) {
		return artist
	}
	var first *acquisitionJob
	for _, j := range a.jobs {
		if j.Kind != "album" || j.Hidden || j.Status == "cancelled" || normalize(j.Title) != normalize(title) || strings.EqualFold(j.Artist, artist) {
			continue
		}
		if named(j.Artist) {
			return j.Artist
		}
		if overlap(credits(j.Artist), credits(artist)) && (first == nil || j.CreatedAt.Before(first.CreatedAt)) {
			first = j
		}
	}
	if first != nil {
		return first.Artist
	}
	return artist
}
