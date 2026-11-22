package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---- Settings ------------------------------------------------------------------------------

// singlesSettings: what the companion does about singles an album makes redundant.
type singlesSettings struct {
	// ReplaceSingles: when an album is added, its songs the library also has as singles are
	// moved out (to <library>/.replaced-singles) and playlists point at the album's tracks.
	ReplaceSingles bool `json:"replace_singles"`
	// AlbumFromSingles: request a whole album once this share of its singles is in the library:
	// "off", "any" (one), "half", or "all".
	AlbumFromSingles string `json:"album_from_singles"`
}

var settingsMu sync.Mutex

func (a *acquisitionService) settingsPath() string {
	return filepath.Join(a.cfg.downloadPath, "glaze", "settings.json")
}

func (a *acquisitionService) loadSettings() singlesSettings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := singlesSettings{AlbumFromSingles: "off"}
	if raw, err := os.ReadFile(a.settingsPath()); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// settings: GET answers the current settings, POST replaces them.
func (a *acquisitionService) settings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.loadSettings())
		return
	}
	var s singlesSettings
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if json.NewDecoder(r.Body).Decode(&s) != nil || (s.AlbumFromSingles != "off" && s.AlbumFromSingles != "any" && s.AlbumFromSingles != "half" && s.AlbumFromSingles != "all") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_settings"})
		return
	}
	raw, _ := json.Marshal(s)
	settingsMu.Lock()
	err := os.WriteFile(a.settingsPath(), raw, 0o600)
	settingsMu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "settings_storage_failed"})
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// afterAdded runs once something new is in the library: an album replaces its singles, and a
// song may complete enough of its album's singles to ask for the album.
func (a *acquisitionService) afterAdded(j acquisitionJob) {
	if a.ctx == nil || a.cfg.libraryPath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
	defer cancel()
	s := a.loadSettings()
	if j.Kind == "album" && s.ReplaceSingles {
		if n, err := a.replaceSingles(ctx, j); err != nil {
			slog.Warn("replace_singles_failed", "job_id", j.ID, "error", err.Error())
		} else if n > 0 {
			slog.Info("replaced_singles", "job_id", j.ID, "songs", n)
		}
	}
	if j.Kind == "track" && s.AlbumFromSingles != "off" {
		a.albumFromSingles(ctx, j, s.AlbumFromSingles)
	}
}

// ---- Navidrome ------------------------------------------------------------------------------

type ndSong struct {
	ID, Title, Artist, Album, AlbumID, Path string
	Artists                                 []struct{ Name string }
}

type ndAlbum struct {
	ID, Name, Artist string
	SongCount        int
	Song             []ndSong
}

func (a *acquisitionService) subsonic(ctx context.Context, route string, params url.Values, out any) error {
	endpoint, err := a.navidromeEndpoint(route)
	if err != nil {
		return err
	}
	if len(params) > 0 {
		endpoint += "&" + params.Encode()
	}
	var envelope struct {
		Subsonic json.RawMessage `json:"subsonic-response"`
	}
	if _, err := a.request(ctx, http.MethodGet, endpoint, nil, nil, &envelope); err != nil {
		return err
	}
	var status struct{ Status string }
	if json.Unmarshal(envelope.Subsonic, &status) != nil || status.Status != "ok" {
		return errors.New("navidrome_" + route + "_failed")
	}
	return json.Unmarshal(envelope.Subsonic, out)
}

func (a *acquisitionService) ndAlbum(ctx context.Context, id string) (ndAlbum, error) {
	var out struct{ Album ndAlbum }
	err := a.subsonic(ctx, "getAlbum", url.Values{"id": {id}}, &out)
	return out.Album, err
}

// ndFindAlbum: the library's album [title] by [artist].
func (a *acquisitionService) ndFindAlbum(ctx context.Context, artist, title string) (ndAlbum, error) {
	var out struct {
		SearchResult3 struct {
			Album []ndAlbum `json:"album"`
		} `json:"searchResult3"`
	}
	if err := a.subsonic(ctx, "search3", url.Values{"query": {stripLabels(title)}, "albumCount": {"20"}, "songCount": {"0"}, "artistCount": {"0"}}, &out); err != nil {
		return ndAlbum{}, err
	}
	for _, al := range out.SearchResult3.Album {
		if baseTitle(al.Name) == baseTitle(title) && overlap(credits(artist), credits(al.Artist)) && isDeluxe(al.Name) == isDeluxe(title) {
			return a.ndAlbum(ctx, al.ID)
		}
	}
	return ndAlbum{}, errors.New("album_not_in_library")
}

func (a *acquisitionService) ndSongs(ctx context.Context, query string) ([]ndSong, error) {
	var out struct {
		SearchResult3 struct {
			Song []ndSong `json:"song"`
		} `json:"searchResult3"`
	}
	err := a.subsonic(ctx, "search3", url.Values{"query": {stripLabels(query)}, "songCount": {"50"}, "albumCount": {"0"}, "artistCount": {"0"}}, &out)
	return out.SearchResult3.Song, err
}

func songCredits(s ndSong) []string {
	names := []string{s.Artist}
	for _, n := range s.Artists {
		names = append(names, n.Name)
	}
	return credits(names...)
}

// ---- Replacing singles ------------------------------------------------------------------------

// singleCopies: for each song of [album], the library's copies of it on single releases
// (a release of at most three songs named after the song, by the same artist).
func (a *acquisitionService) singleCopies(ctx context.Context, album ndAlbum) (map[string]ndSong, error) {
	albums := map[string]ndAlbum{}
	found := map[string]ndSong{} // single song id -> the album's song that replaces it
	for _, song := range album.Song {
		candidates, err := a.ndSongs(ctx, song.Title)
		if err != nil {
			return nil, err
		}
		for _, c := range candidates {
			if c.AlbumID == album.ID || c.ID == song.ID || baseTitle(c.Title) != baseTitle(song.Title) ||
				!overlap(songCredits(c), songCredits(song)) || baseTitle(c.Album) != baseTitle(c.Title) {
				continue
			}
			release, ok := albums[c.AlbumID]
			if !ok {
				if release, err = a.ndAlbum(ctx, c.AlbumID); err != nil {
					continue
				}
				albums[c.AlbumID] = release
			}
			if release.SongCount > 0 && release.SongCount <= 3 {
				found[c.ID] = song
				found["path:"+c.ID] = c
			}
		}
	}
	return found, nil
}

// replaceSingles moves out the single copies of the album's songs, then points every playlist
// that had them at the album's tracks. It returns how many songs it replaced.
func (a *acquisitionService) replaceSingles(ctx context.Context, j acquisitionJob) (int, error) {
	album, err := a.ndFindAlbum(ctx, j.Artist, j.Title)
	if err != nil {
		return 0, err
	}
	copies, err := a.singleCopies(ctx, album)
	if err != nil {
		return 0, err
	}
	replacement := map[string]string{} // single song id -> album song id
	for id, song := range copies {
		if !strings.HasPrefix(id, "path:") {
			replacement[id] = song.ID
		}
	}
	if len(replacement) == 0 {
		return 0, nil
	}
	// Playlists are read before anything moves: after the rescan the singles are gone from them.
	playlists, err := a.playlistsWith(ctx, replacement)
	if err != nil {
		return 0, err
	}
	trash := filepath.Join(a.cfg.libraryPath, ".replaced-singles", time.Now().UTC().Format("2006-01-02"))
	moved := 0
	for id := range replacement {
		if err := a.moveOut(copies["path:"+id].Path, trash); err != nil {
			slog.Warn("single_not_moved", "song", id, "error", err.Error())
			delete(replacement, id)
			continue
		}
		moved++
	}
	if moved == 0 {
		return 0, nil
	}
	if err := a.scanNavidrome(ctx); err != nil {
		return moved, err
	}
	return moved, a.repointPlaylists(ctx, playlists, replacement)
}

// moveOut moves one library file (its path as Navidrome reports it) into [trash], and the rest
// of its folder too when no audio is left in it. Hidden folders aren't scanned by Navidrome.
func (a *acquisitionService) moveOut(rel, trash string) error {
	root, err := filepath.Abs(a.cfg.libraryPath)
	if err != nil {
		return err
	}
	src := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if rel == "" || !strings.HasPrefix(src, root+string(os.PathSeparator)) {
		return errors.New("outside_library")
	}
	if _, err := os.Stat(src); err != nil {
		return errors.New("file_missing")
	}
	dest := filepath.Join(trash, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := moveFile(src, dest); err != nil {
		return err
	}
	dir := filepath.Dir(src)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && isAudioName(e.Name()) {
			return nil
		}
	}
	// Only artwork and the like left: the release folder goes with it.
	for _, e := range entries {
		if !e.IsDir() {
			_ = moveFile(filepath.Join(dir, e.Name()), filepath.Join(filepath.Dir(dest), e.Name()))
		}
	}
	_ = os.Remove(dir)
	return nil
}

func isAudioName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".flac", ".mp3", ".m4a", ".aac", ".ogg", ".opus", ".wav", ".aiff", ".aif", ".alac", ".wma":
		return true
	}
	return false
}

// moveFile renames, or copies and removes when the trash is on another disk.
func moveFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	if err := writeAll(dest, in); err != nil {
		in.Close()
		return err
	}
	in.Close()
	return os.Remove(src)
}

type playlistEntries struct {
	ID    string
	Songs []string
}

// playlistsWith: every playlist holding one of [ids], with its full running order.
func (a *acquisitionService) playlistsWith(ctx context.Context, ids map[string]string) ([]playlistEntries, error) {
	var list struct {
		Playlists struct {
			Playlist []struct{ ID string } `json:"playlist"`
		} `json:"playlists"`
	}
	if err := a.subsonic(ctx, "getPlaylists", nil, &list); err != nil {
		return nil, err
	}
	var out []playlistEntries
	for _, p := range list.Playlists.Playlist {
		var detail struct {
			Playlist struct {
				Entry []struct{ ID string } `json:"entry"`
			} `json:"playlist"`
		}
		if err := a.subsonic(ctx, "getPlaylist", url.Values{"id": {p.ID}}, &detail); err != nil {
			continue
		}
		entries := playlistEntries{ID: p.ID}
		hit := false
		for _, e := range detail.Playlist.Entry {
			entries.Songs = append(entries.Songs, e.ID)
			_, ok := ids[e.ID]
			hit = hit || ok
		}
		if hit {
			out = append(out, entries)
		}
	}
	return out, nil
}

// repointPlaylists rewrites each playlist in its own order with the singles swapped for the
// album's tracks (createPlaylist with an id replaces that playlist's songs).
func (a *acquisitionService) repointPlaylists(ctx context.Context, playlists []playlistEntries, replacement map[string]string) error {
	var failed error
	for _, p := range playlists {
		params := url.Values{"playlistId": {p.ID}}
		for _, id := range p.Songs {
			if to, ok := replacement[id]; ok {
				id = to
			}
			params.Add("songId", id)
		}
		var ignored struct{}
		if err := a.subsonic(ctx, "createPlaylist", params, &ignored); err != nil {
			failed = fmt.Errorf("playlist %s: %w", p.ID, err)
		}
	}
	return failed
}

// ---- Whole album from its singles ---------------------------------------------------------

type dzAlbum struct {
	ID          json.Number `json:"id"`
	Title       string      `json:"title"`
	RecordType  string      `json:"record_type"`
	ReleaseDate string      `json:"release_date"`
	Artist      struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
	} `json:"artist"`
	Tracks struct {
		Data []struct {
			Title string `json:"title"`
		} `json:"data"`
	} `json:"tracks"`
}

// parentAlbum: the album (not a single or EP) the song [j] is on, per Deezer.
func (a *acquisitionService) parentAlbum(ctx context.Context, j acquisitionJob) (dzAlbum, error) {
	var found struct {
		Data []struct {
			Title  string `json:"title"`
			Artist struct{ Name string }
			Album  struct {
				ID json.Number `json:"id"`
			} `json:"album"`
		} `json:"data"`
	}
	q := cleanArtist(j.Artist) + " " + j.Title
	if _, err := a.request(ctx, http.MethodGet, "https://api.deezer.com/search/track?limit=25&q="+url.QueryEscape(q), nil, nil, &found); err != nil {
		return dzAlbum{}, err
	}
	today := time.Now().Format("2006-01-02")
	seen := map[string]bool{}
	for _, t := range found.Data {
		id := t.Album.ID.String()
		if seen[id] || baseTitle(t.Title) != baseTitle(j.Title) || !overlap(credits(j.Artist), credits(t.Artist.Name)) {
			continue
		}
		seen[id] = true
		var album dzAlbum
		if _, err := a.request(ctx, http.MethodGet, "https://api.deezer.com/album/"+url.PathEscape(id), nil, nil, &album); err != nil {
			continue
		}
		if album.RecordType == "album" && album.ReleaseDate != "" && album.ReleaseDate <= today && len(album.Tracks.Data) > 3 {
			return album, nil
		}
	}
	return dzAlbum{}, errors.New("no_parent_album")
}

// albumFromSingles asks for the whole album once enough of its singles are in the library:
// "any" one, "half" of them, or "all".
func (a *acquisitionService) albumFromSingles(ctx context.Context, j acquisitionJob, mode string) {
	album, err := a.parentAlbum(ctx, j)
	if err != nil {
		return
	}
	if _, err := a.ndFindAlbum(ctx, album.Artist.Name, album.Title); err == nil {
		return // already have it
	}
	tracks := map[string]bool{}
	for _, t := range album.Tracks.Data {
		tracks[baseTitle(t.Title)] = true
	}
	var releases struct {
		Data []dzAlbum `json:"data"`
	}
	if _, err := a.request(ctx, http.MethodGet, "https://api.deezer.com/artist/"+url.PathEscape(album.Artist.ID.String())+"/albums?limit=100", nil, nil, &releases); err != nil {
		return
	}
	// The album's singles: single releases named after one of its songs.
	singles := map[string]bool{}
	for _, r := range releases.Data {
		if r.RecordType == "single" && tracks[baseTitle(r.Title)] {
			singles[baseTitle(r.Title)] = true
		}
	}
	singles[baseTitle(j.Title)] = singles[baseTitle(j.Title)] || tracks[baseTitle(j.Title)]
	owned := 0
	for title := range singles {
		songs, err := a.ndSongs(ctx, title)
		if err != nil {
			return
		}
		for _, s := range songs {
			if baseTitle(s.Title) == title && overlap(songCredits(s), credits(album.Artist.Name)) {
				owned++
				break
			}
		}
	}
	enough := map[string]bool{"any": owned >= 1, "half": owned*2 >= len(singles), "all": owned == len(singles)}[mode]
	if !enough || owned == 0 {
		return
	}
	if job, err := a.enqueueCatalogue(album.Artist.Name, album.Title, "album", album.ID.String(), ""); err == nil {
		slog.Info("album_from_singles", "job_id", job.ID, "owned", owned, "singles", len(singles))
	}
}
