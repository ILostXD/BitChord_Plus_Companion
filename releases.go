package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type releaseTrack struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"`
	Explicit bool   `json:"explicit"`
	Released bool   `json:"released"`
}
type upcomingRelease struct {
	ID          string         `json:"id"`
	ArtistID    string         `json:"artistId"`
	Title       string         `json:"title"`
	Artist      string         `json:"artist"`
	Date        string         `json:"releaseDate"`
	ReleaseAt   int64          `json:"releaseAt"`
	Cover       string         `json:"cover"`
	TrackCount  int            `json:"trackCount"`
	Tracks      []releaseTrack `json:"tracks"`
	Saved       bool           `json:"saved"`
	Followed    bool           `json:"followed"`
	JobID       string         `json:"jobId"`
	Status      string         `json:"status"`
	Error       string         `json:"error"`
	CheckedAt   int64          `json:"checkedAt,omitempty"`
	NextCheckAt int64          `json:"nextCheckAt,omitempty"`
	// EarlyJobs: track id -> the job that fetched it before the album was fully out.
	EarlyJobs map[string]string `json:"earlyJobs,omitempty"`
}
type releaseArtist struct{ ID, Name string }
type releaseData struct {
	Albums  map[string]upcomingRelease `json:"albums"`
	Checked map[string]time.Time       `json:"checked"`
	Saves   map[string]map[string]bool `json:"presaves"`
}
type releaseService struct {
	a         *acquisitionService
	mu        sync.Mutex
	refreshMu sync.Mutex
	path      string
	data      releaseData
	wake      chan struct{}
	pending   map[string]releaseArtist
	appleLast time.Time
}

func registerReleases(mux *http.ServeMux, ctx context.Context, a *acquisitionService) {
	statePath := a.cfg.statePath
	if statePath == "" {
		statePath = filepath.Join(a.cfg.downloadPath, "glaze")
	}
	s := &releaseService{a: a, path: filepath.Join(statePath, "upcoming-albums.json"),
		data: releaseData{Albums: map[string]upcomingRelease{}, Checked: map[string]time.Time{}}, wake: make(chan struct{}, 1)}
	if a.cfg.statePath != "" {
		_ = os.MkdirAll(a.cfg.statePath, 0700)
	}
	if a.cfg.statePath != "" || a.cfg.downloadPath != "" {
		raw, err := os.ReadFile(s.path)
		if err == nil {
			err = json.Unmarshal(raw, &s.data)
		}
		if (err != nil && !os.IsNotExist(err)) || s.data.Albums == nil || s.data.Checked == nil {
			// Do not overwrite damaged or unreadable pre-save storage.
			mux.HandleFunc("/api/v1/releases", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 503, map[string]string{"error": "release_storage_unavailable"})
			})
			mux.HandleFunc("/api/v1/releases/", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 503, map[string]string{"error": "release_storage_unavailable"})
			})
			return
		}
	}
	// Search API timestamps describe catalogue dates, not verified streaming launch times.
	for id, album := range s.data.Albums {
		if day, err := time.Parse("2006-01-02", album.Date); err == nil {
			album.ReleaseAt = day.UnixMilli()
			s.data.Albums[id] = album
		}
	}
	s.initializeSaves()
	mux.HandleFunc("GET /api/v1/releases", s.list)
	s.data.Checked = map[string]time.Time{} // Refresh catalogue metadata after a server restart; keep explicit saves.
	mux.HandleFunc("GET /api/v1/releases/{id}", s.detail)
	mux.HandleFunc("PUT /api/v1/releases/{id}/presave", s.save)
	mux.HandleFunc("POST /api/v1/releases/{id}/retry", s.retry)
	if (a.cfg.statePath != "" || a.cfg.downloadPath != "") && a.cfg.navidromeUsername != "" {
		go s.run(ctx)
	}
}

// Legacy saves belong to the configured owner, never to another account.
func (s *releaseService) initializeSaves() {
	if s.data.Saves == nil {
		s.data.Saves = map[string]map[string]bool{}
		owner := strings.ToLower(s.a.cfg.navidromeUsername)
		for id, album := range s.data.Albums {
			if album.Saved && owner != "" {
				s.setSaved(owner, id, true)
			}
		}
	}
	for id, album := range s.data.Albums {
		album.Saved, album.Followed = false, false
		s.data.Albums[id] = album
	}
}

// Caller holds mu (except initialization before serving requests).
func (s *releaseService) setSaved(user, id string, saved bool) {
	user = strings.ToLower(user)
	if saved {
		if s.data.Saves[user] == nil {
			s.data.Saves[user] = map[string]bool{}
		}
		s.data.Saves[user][id] = true
	} else {
		delete(s.data.Saves[user], id)
		if len(s.data.Saves[user]) == 0 {
			delete(s.data.Saves, user)
		}
	}
}

func (s *releaseService) anySaved(id string) bool {
	for _, saved := range s.data.Saves {
		if saved[id] {
			return true
		}
	}
	return false
}

func (s *releaseService) userAlbum(album upcomingRelease, user string) upcomingRelease {
	album.Saved = s.data.Saves[strings.ToLower(user)][album.ID]
	album.Followed = false
	return s.jobState(album)
}

// Caller holds mu. Atomic persistence also preserves pre-saves across restarts.
func (s *releaseService) persist() error {
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(s.path+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(s.path+".tmp", s.path)
}
func (s *releaseService) artists(ctx context.Context, id string) ([]releaseArtist, error) {
	route := "getStarred2"
	if id != "" {
		route = "getArtist"
	}
	endpoint, err := s.a.navidromeEndpoint(route)
	if identity, ok := ctx.Value(navidromeIdentityKey{}).(navidromeIdentity); ok {
		endpoint, err = identity.endpoint(s.a.cfg, route), nil
	}
	if err != nil {
		return nil, err
	}
	if id != "" {
		endpoint += "&id=" + url.QueryEscape(id)
	}
	var response struct {
		Subsonic struct {
			Status  string
			Starred struct {
				Artists []releaseArtist `json:"artist"`
			} `json:"starred2"`
			Artist releaseArtist
		} `json:"subsonic-response"`
	}
	_, err = s.a.request(ctx, http.MethodGet, endpoint, nil, nil, &response)
	if err != nil || response.Subsonic.Status != "ok" {
		return nil, errors.New("favorites_unavailable")
	}
	if id != "" {
		if response.Subsonic.Artist.ID == "" {
			return nil, errors.New("artist_not_found")
		}
		return []releaseArtist{response.Subsonic.Artist}, nil
	}
	return response.Subsonic.Starred.Artists, nil
}
func (s *releaseService) list(w http.ResponseWriter, r *http.Request) {
	artistID := r.URL.Query().Get("artist_id")
	artistName := strings.TrimSpace(r.URL.Query().Get("artist_name"))
	if len(artistID) > 128 || len(artistName) > 128 {
		writeJSON(w, 400, map[string]string{"error": "invalid_artist"})
		return
	}
	// Any artist by name (an artist page in the app), whether or not the server has them.
	if artistName != "" && artistID == "" {
		s.byName(w, r, artistName)
		return
	}
	artists, err := s.artists(r.Context(), artistID)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "favorites_unavailable"})
		return
	}
	if artistID != "" {
		if err = s.refresh(r.Context(), artists[0]); err != nil {
			writeJSON(w, 502, map[string]string{"error": "catalogue_unavailable"})
			return
		}
	} else {
		s.mu.Lock()
		if s.pending == nil {
			s.pending = map[string]releaseArtist{}
		}
		for _, artist := range artists {
			s.pending[artist.ID] = artist
		}
		s.mu.Unlock()
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	ids := map[string]bool{}
	for _, artist := range artists {
		ids[artist.ID] = true
	}
	s.mu.Lock()
	out := []upcomingRelease{}
	for _, album := range s.data.Albums {
		album = s.userAlbum(album, companionUsername(r.Context(), s.a.cfg))
		album.Followed = ids[album.ArtistID]
		if (ids[album.ArtistID] && album.ReleaseAt > time.Now().UnixMilli()) || (artistID == "" && album.Saved) {
			out = append(out, album)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ReleaseAt < out[j].ReleaseAt })
	writeJSON(w, 200, out)
}
func (s *releaseService) detail(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cached, exists := s.data.Albums[r.PathValue("id")]
	s.mu.Unlock()
	if !exists {
		// Explicit catalogue selection bypasses artist-name discovery. IDs are
		// looked up at Apple; callers cannot supply arbitrary URLs or metadata.
		if err := s.registerCatalogue(r.Context(), r.PathValue("id")); err != nil {
			writeJSON(w, 404, map[string]string{"error": "release_not_found"})
			return
		}
	}
	if exists {
		// The known details are answered at once; the catalogue is checked again behind them
		// (a failed refresh never discards the last good details).
		go func() {
			if cached.ArtistID == "" {
				_ = s.registerCatalogue(s.bg(), cached.ID)
			} else {
				_ = s.refresh(s.bg(), releaseArtist{cached.ArtistID, cached.Artist})
			}
		}()
	}
	s.mu.Lock()
	album, ok := s.data.Albums[r.PathValue("id")]
	if ok {
		album = s.userAlbum(album, companionUsername(r.Context(), s.a.cfg))
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "release_not_found"})
		return
	}
	writeJSON(w, 200, album)
}
func (s *releaseService) save(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Saved *bool `json:"saved"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || input.Saved == nil {
		writeJSON(w, 400, map[string]string{"error": "invalid_presave"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	album, ok := s.data.Albums[id]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "release_not_found"})
		return
	}
	if album.JobID != "" || album.ReleaseAt <= time.Now().UnixMilli() {
		writeJSON(w, 409, map[string]string{"error": "release_already_started"})
		return
	}
	if s.a.cfg.downloadPath == "" {
		writeJSON(w, 503, map[string]string{"error": "downloads_disabled"})
		return
	}
	user := companionUsername(r.Context(), s.a.cfg)
	if user == "" {
		writeJSON(w, 503, map[string]string{"error": "account_not_configured"})
		return
	}
	previous := s.data.Saves[strings.ToLower(user)][id]
	s.setSaved(user, id, *input.Saved)
	if s.persist() != nil {
		s.setSaved(user, id, previous)
		writeJSON(w, 500, map[string]string{"error": "presave_storage_failed"})
		return
	}
	writeJSON(w, 200, s.userAlbum(album, user))
}
func (s *releaseService) jobState(album upcomingRelease) upcomingRelease {
	if album.JobID != "" {
		s.a.mu.Lock()
		if job := s.a.jobs[album.JobID]; job != nil {
			album.Status, album.Error = job.Status, job.Error
			album.CheckedAt = job.UpdatedAt.UnixMilli()
			album.NextCheckAt = 0
			if job.Status == "failed" && job.Error != "slskd_enqueue_failed" {
				album.NextCheckAt = job.UpdatedAt.Add(releaseRetryInterval(album.ReleaseAt, time.Now())).UnixMilli()
			}
		}
		s.a.mu.Unlock()
	}
	return album
}

func releaseRetryInterval(releaseAt int64, now time.Time) time.Duration {
	if now.Sub(time.UnixMilli(releaseAt)) < 24*time.Hour {
		return time.Hour
	}
	return 24 * time.Hour
}

func (s *releaseService) retry(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	album, ok := s.data.Albums[id]
	if !ok || !s.data.Saves[strings.ToLower(companionUsername(r.Context(), s.a.cfg))][id] {
		writeJSON(w, 404, map[string]string{"error": "presave_not_found"})
		return
	}
	album = s.jobState(album)
	if album.ReleaseAt > time.Now().UnixMilli() || album.Status != "failed" && album.Status != "cancelled" || album.Error == "slskd_enqueue_failed" {
		writeJSON(w, 409, map[string]string{"error": "release_not_ready_to_retry"})
		return
	}
	job, err := s.a.enqueue(album.Artist, album.Title, "album")
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "release_retry_failed"})
		return
	}
	album.JobID = job.ID
	previous := s.data.Albums[id]
	s.data.Albums[id] = album
	if err := s.persist(); err != nil {
		s.data.Albums[id] = previous
		writeJSON(w, 500, map[string]string{"error": "presave_storage_failed"})
		return
	}
	writeJSON(w, 200, s.userAlbum(album, companionUsername(r.Context(), s.a.cfg)))
}

func (s *releaseService) registerCatalogue(ctx context.Context, id string) error {
	if len(id) == 0 || len(id) > 20 || strings.Trim(id, "0123456789") != "" {
		return errors.New("invalid_catalogue_id")
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	previous, exists := s.data.Albums[id]
	checked := s.data.Checked["catalogue:"+id]
	s.mu.Unlock()
	if exists && time.Since(checked) < 6*time.Hour {
		return nil
	}
	entries, err := s.apple(ctx, "lookup", url.Values{"id": {id}, "entity": {"song"}, "limit": {"200"}})
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.WrapperType != "collection" || fmt.Sprint(entry.CollectionID) != id {
			continue
		}
		album := catalogueAlbum(entry, entries, "", time.Now())
		if !exists && album.ReleaseAt <= time.Now().UnixMilli() || !safeName(album.Artist) || !safeName(album.Title) {
			return errors.New("not_upcoming")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.data.Albums[id]; ok {
			album.ArtistID, album.JobID, album.Status, album.Error = current.ArtistID, current.JobID, current.Status, current.Error
			album.CheckedAt, album.NextCheckAt = current.CheckedAt, current.NextCheckAt
		}
		s.data.Albums[id] = album
		s.data.Checked["catalogue:"+id] = time.Now()
		if err := s.persist(); err != nil {
			if exists {
				s.data.Albums[id] = previous
			} else {
				delete(s.data.Albums, id)
			}
			s.data.Checked["catalogue:"+id] = checked
			return err
		}
		return nil
	}
	return errors.New("release_not_found")
}

type itunesEntry struct {
	WrapperType, Kind, ArtistName, CollectionName, TrackName, ReleaseDate, ArtworkURL100, TrackExplicitness, PreviewURL string
	ArtistID, CollectionID, TrackID                                                                                     int64
	TrackCount, TrackTimeMillis, TrackNumber, DiscNumber                                                                int
	IsStreamable                                                                                                        bool
}

func (s *releaseService) apple(ctx context.Context, route string, query url.Values) ([]itunesEntry, error) {
	// ponytail: one serial catalogue worker; 3 seconds between requests stays below Apple's ~20/minute limit.
	if delay := time.Until(s.appleLast.Add(3 * time.Second)); delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	s.appleLast = time.Now()
	var response struct{ Results []itunesEntry }
	_, err := s.a.request(ctx, http.MethodGet, "https://itunes.apple.com/"+route+"?"+query.Encode(), nil, nil, &response)
	return response.Results, err
}
func (s *releaseService) refresh(ctx context.Context, artist releaseArtist) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	checked := s.data.Checked[artist.ID]
	ttl := 6 * time.Hour
	known := map[string]bool{}
	for id, album := range s.data.Albums {
		if album.ArtistID == artist.ID {
			known[id] = true
			if delta := time.Until(time.UnixMilli(album.ReleaseAt)); delta > -24*time.Hour && delta < 24*time.Hour {
				ttl = 10 * time.Minute
			}
		}
	}
	s.mu.Unlock()
	if time.Since(checked) < ttl {
		return nil
	}
	entries, err := s.apple(ctx, "search", url.Values{"term": {artist.Name}, "entity": {"musicArtist"}, "limit": {"20"}})
	if err != nil {
		return err
	}
	var matches []int64
	for _, entry := range entries {
		if strings.EqualFold(strings.TrimSpace(entry.ArtistName), strings.TrimSpace(artist.Name)) {
			matches = append(matches, entry.ArtistID)
		}
	}
	if len(matches) != 1 {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.data.Checked[artist.ID] = time.Now()
		return s.persist()
	} // Ambiguous names are skipped rather than following the wrong artist.
	entries, err = s.apple(ctx, "lookup", url.Values{"id": {fmt.Sprint(matches[0])}, "entity": {"album"}, "limit": {"200"}})
	if err != nil {
		return err
	}
	albums := []upcomingRelease{}
	for _, entry := range entries {
		at, parseErr := time.Parse(time.RFC3339, entry.ReleaseDate)
		if entry.WrapperType != "collection" || entry.ArtistID != matches[0] || parseErr != nil || !at.After(time.Now()) && !known[fmt.Sprint(entry.CollectionID)] || strings.HasSuffix(entry.CollectionName, " - Single") {
			continue
		}
		tracks, lookupErr := s.apple(ctx, "lookup", url.Values{"id": {fmt.Sprint(entry.CollectionID)}, "entity": {"song"}, "limit": {"200"}})
		if lookupErr != nil {
			return lookupErr
		}
		album := catalogueAlbum(entry, tracks, artist.ID, time.Now())
		albums = append(albums, album)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, _ := json.Marshal(s.data)
	for _, album := range albums {
		old := s.data.Albums[album.ID]
		// A lookup by name doesn't take an album away from the server artist it was found for.
		if old.ArtistID != "" && strings.HasPrefix(album.ArtistID, "name:") {
			album.ArtistID = old.ArtistID
		}
		album.JobID = old.JobID
		album.Status, album.Error, album.CheckedAt, album.NextCheckAt = old.Status, old.Error, old.CheckedAt, old.NextCheckAt
		s.data.Albums[album.ID] = album
	}
	// Remove withdrawn announcements, while retaining explicit pre-saves and released jobs.
	found := map[string]bool{}
	for _, album := range albums {
		found[album.ID] = true
	}
	for id, album := range s.data.Albums {
		if album.ArtistID == artist.ID && !s.anySaved(id) && album.JobID == "" && !found[id] {
			delete(s.data.Albums, id)
		}
	}
	s.data.Checked[artist.ID] = time.Now()
	if err = s.persist(); err != nil {
		var restored releaseData
		_ = json.Unmarshal(previous, &restored)
		s.data = restored
		return err
	}
	return nil
}
func catalogueAlbum(entry itunesEntry, tracks []itunesEntry, artistID string, now time.Time) upcomingRelease {
	timestamp, _ := time.Parse(time.RFC3339, entry.ReleaseDate)
	// Apple Search supplies a catalogue release date; its time component isn't a launch guarantee.
	at, _ := time.Parse("2006-01-02", timestamp.Format("2006-01-02"))
	cover := ""
	if u, err := url.Parse(entry.ArtworkURL100); err == nil && u.Scheme == "https" && u.User == nil && strings.HasSuffix(u.Hostname(), ".mzstatic.com") && (u.Port() == "" || u.Port() == "443") {
		cover = strings.Replace(entry.ArtworkURL100, "100x100bb", "800x800bb", 1)
	}
	album := upcomingRelease{ID: fmt.Sprint(entry.CollectionID), ArtistID: artistID, Title: entry.CollectionName, Artist: entry.ArtistName,
		Date: at.Format("2006-01-02"), ReleaseAt: at.UnixMilli(), Cover: cover, TrackCount: entry.TrackCount, Tracks: []releaseTrack{}}
	sort.SliceStable(tracks, func(i, j int) bool {
		if tracks[i].DiscNumber != tracks[j].DiscNumber {
			return tracks[i].DiscNumber < tracks[j].DiscNumber
		}
		return tracks[i].TrackNumber < tracks[j].TrackNumber
	})
	for _, track := range tracks {
		if track.Kind != "song" || track.CollectionID != entry.CollectionID {
			continue
		}
		released, err := time.Parse(time.RFC3339, track.ReleaseDate)
		preview, previewErr := url.Parse(track.PreviewURL)
		available := previewErr == nil && preview.Scheme == "https" && preview.User == nil &&
			(strings.HasSuffix(preview.Hostname(), ".itunes.apple.com") || strings.HasSuffix(preview.Hostname(), ".mzstatic.com"))
		album.Tracks = append(album.Tracks, releaseTrack{ID: fmt.Sprint(track.TrackID), Title: track.TrackName, Artist: track.ArtistName,
			Duration: track.TrackTimeMillis / 1000, Explicit: track.TrackExplicitness == "explicit", Released: track.IsStreamable && available && (err == nil && !released.After(now) || err != nil && !at.After(now))})
	}
	return album
}
func (s *releaseService) acquireDue() {
	if s.a.cfg.downloadPath == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, album := range s.data.Albums {
		if !s.anySaved(id) {
			continue
		}
		// Before release day, the tracks that are already out are fetched as they appear.
		if album.ReleaseAt > time.Now().UnixMilli() {
			if s.requestReleasedTracks(&album) {
				s.data.Albums[id] = album
				_ = s.persist()
			}
			continue
		}
		// On release day, an album that was started track by track is finished the same way,
		// rather than downloaded whole on top of what the library has.
		if len(album.EarlyJobs) > 0 && album.JobID == "" {
			if s.requestRemainingTracks(&album) {
				s.data.Albums[id] = album
				_ = s.persist()
			}
			continue
		}
		album = s.jobState(album)
		if album.JobID == "" && album.NextCheckAt > time.Now().UnixMilli() {
			continue
		}
		if album.JobID != "" {
			if album.Status != "failed" || album.Error == "slskd_enqueue_failed" {
				continue
			}
			s.a.mu.Lock()
			job := s.a.jobs[album.JobID]
			interval := releaseRetryInterval(album.ReleaseAt, time.Now())
			retry := job != nil && time.Since(job.UpdatedAt) >= interval
			s.a.mu.Unlock()
			if !retry {
				continue
			}
		}
		// A retry must get past the day-long repeat suppression, or the hourly release-day retries do nothing.
		job, err := s.a.enqueueCatalogueOpt(album.Artist, album.Title, "album", "", "", "", album.JobID != "")
		if err != nil {
			album.Status, album.Error, album.CheckedAt = "failed", "Waiting for a matching source or catalogue", time.Now().UnixMilli()
			album.NextCheckAt = time.Now().Add(releaseRetryInterval(album.ReleaseAt, time.Now())).UnixMilli()
			s.data.Albums[id] = album
			_ = s.persist()
			continue
		}
		album.JobID = job.ID
		album.Status, album.Error, album.NextCheckAt = "searching", "", 0
		s.data.Albums[id] = album
		_ = s.persist() // enqueue itself is persisted and deduplicated if this write fails.
	}
}
func (s *releaseService) run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var checked time.Time
	for {
		if time.Since(checked) >= time.Hour {
			if artists, err := s.artists(ctx, ""); err == nil {
				for _, artist := range artists {
					if ctx.Err() != nil {
						return
					}
					_ = s.refresh(ctx, artist)
				}
				checked = time.Now()
			}
		}
		s.mu.Lock()
		pending := s.pending
		s.pending = nil
		s.mu.Unlock()
		for _, artist := range pending {
			if ctx.Err() != nil {
				return
			}
			_ = s.refresh(ctx, artist)
		}
		s.acquireDue()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// requestReleasedTracks asks for each track of a Pre-Saved album that is already out and not yet
// asked for, a few per pass. A track the library already has is recorded as such, not downloaded.
// It reports whether anything changed.
func (s *releaseService) requestReleasedTracks(album *upcomingRelease) bool {
	changed, asked := false, 0
	for _, t := range album.Tracks {
		if !t.Released || album.EarlyJobs[t.ID] != "" {
			continue
		}
		if asked++; asked > 3 {
			break
		}
		job := s.a.recordLibraryDecision(context.Background(), t.Artist, t.Title, "track", album.Title, t.Duration)
		if job == nil {
			var err error
			if job, err = s.a.enqueueCatalogue(t.Artist, t.Title, "track", "", album.Title); err != nil {
				continue
			}
		}
		if album.EarlyJobs == nil {
			album.EarlyJobs = map[string]string{}
		}
		album.EarlyJobs[t.ID] = job.ID
		changed = true
	}
	return changed
}

// requestRemainingTracks finishes an album that was fetched track by track: every track without a
// job, or whose job failed, is asked for now. It then points the album at its newest job, so the
// status the app shows follows the last of them.
func (s *releaseService) requestRemainingTracks(album *upcomingRelease) bool {
	var last string
	for _, t := range album.Tracks {
		if id := album.EarlyJobs[t.ID]; id != "" {
			s.a.mu.Lock()
			job := s.a.jobs[id]
			failed := job != nil && job.Status == "failed" && job.Error != "slskd_enqueue_failed"
			s.a.mu.Unlock()
			if !failed {
				last = id
				continue
			}
		}
		job := s.a.recordLibraryDecisionOpt(context.Background(), t.Artist, t.Title, "track", album.Title, t.Duration, true)
		if job == nil {
			var err error
			if job, err = s.a.enqueueCatalogueOpt(t.Artist, t.Title, "track", "", album.Title, album.Artist, true); err != nil {
				continue
			}
		}
		album.EarlyJobs[t.ID] = job.ID
		last = job.ID
	}
	if last == "" {
		return false
	}
	album.JobID, album.Status, album.Error = last, "searching", ""
	return true
}

// byName lists the upcoming albums of the artist called [name] (Apple's catalogue, checked at
// most every few hours), for an artist page that isn't one of the server's starred artists.
func (s *releaseService) byName(w http.ResponseWriter, r *http.Request, name string) {
	id := "name:" + normalize(name)
	// Answered from what is known at once; a stale artist is checked again in the background,
	// and one never looked up is waited for only briefly (the app shows it when it can).
	s.mu.Lock()
	checked, known := s.data.Checked[id]
	s.mu.Unlock()
	if !known {
		done := make(chan struct{})
		go func() { _ = s.refresh(s.bg(), releaseArtist{id, name}); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	} else if time.Since(checked) > 6*time.Hour {
		go func() { _ = s.refresh(s.bg(), releaseArtist{id, name}) }()
	}
	s.mu.Lock()
	out := []upcomingRelease{}
	for _, album := range s.data.Albums {
		if (album.ArtistID == id || normalize(album.Artist) == normalize(name)) && album.ReleaseAt > time.Now().UnixMilli() {
			out = append(out, s.userAlbum(album, companionUsername(r.Context(), s.a.cfg)))
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ReleaseAt < out[j].ReleaseAt })
	writeJSON(w, 200, out)
}

// bg is the context background catalogue checks run under: the service's own, outliving
// the request that started them.
func (s *releaseService) bg() context.Context {
	if s.a != nil && s.a.ctx != nil {
		return s.a.ctx
	}
	return context.Background()
}
