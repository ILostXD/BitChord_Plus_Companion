package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fillLossy is the app's optional fallback for a song no lossless copy was found for: it names
// that song's JioSaavn stream, and the companion downloads it, tags it and adds it to the
// library. The companion itself never picks a lossy copy; only this explicit request adds one.
// POST /api/v1/acquisition/jobs/{jobId}/lossy {"url": "https://…saavncdn.com/…", "ext": "m4a"}
// An album comes one song per call, each naming its title, artist and track number; the call
// marked "last" rescans the library and closes the request.
func (a *acquisitionService) fillLossy(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL, Ext, Album, AlbumArtist string
		Title, Artist                string
		Track, Disc                  int
		Last                         bool
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	source, err := url.Parse(input.URL)
	// JioSaavn's CDN only: this must not become a way to fetch arbitrary addresses.
	if err != nil || source.Scheme != "https" || source.User != nil || !strings.HasSuffix(source.Hostname(), ".saavncdn.com") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_source"})
		return
	}
	ext := strings.ToLower(strings.TrimPrefix(input.Ext, "."))
	if ext == "mp4" || ext == "" {
		ext = "m4a"
	}
	if ext != "m4a" && ext != "mp3" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_format"})
		return
	}
	if a.cfg.libraryPath == "" || a.cfg.downloadPath == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "library_unavailable"})
		return
	}
	a.mu.Lock()
	current := a.jobs[r.PathValue("jobId")]
	var job acquisitionJob
	if current != nil {
		job = *current
	}
	a.mu.Unlock()
	if current == nil || job.Status != "failed" || job.Error != "no_suitable_slskd_match" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not_a_failed_request"})
		return
	}
	song := repairTrack{title: job.Title, artist: job.Artist, track: 1, disc: 1}
	if job.Kind == "album" {
		// The album is the request; each call is one of its songs.
		input.Album, input.AlbumArtist = job.Title, job.Artist
		song = repairTrack{title: strings.TrimSpace(input.Title), artist: strings.TrimSpace(input.Artist), track: max(input.Track, 1), disc: max(input.Disc, 1)}
		if song.artist == "" {
			song.artist = job.Artist
		}
		if !safeName(song.title) || !safeName(song.artist) || input.Track > 999 || input.Disc > 99 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
	}
	album := strings.TrimSpace(input.Album)
	if album == "" {
		album = job.AlbumTitle
	}
	if album == "" {
		album = job.Title
	}
	albumArtist := strings.TrimSpace(input.AlbumArtist)
	if albumArtist == "" {
		albumArtist = job.AlbumArtist
	}
	if albumArtist == "" {
		albumArtist = job.Artist
	}
	if !safeName(album) || !safeName(albumArtist) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	song.album = album
	dest, err := a.addLossy(r, source.String(), ext, job.ID, song, albumArtist)
	if err != nil && !(job.Kind == "album" && input.Last && err.Error() == "already_in_library") {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if job.Kind == "album" && !input.Last {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "added"})
		return
	}
	if err := a.scanNavidrome(r.Context()); err != nil {
		if job.Kind == "track" && dest != "" {
			_ = os.Remove(dest)
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "scan_failed"})
		return
	}
	job.Status, job.Error, job.Source = "rescanned", "", "jiosaavn"
	job.ReviewSummary = "Lossy copy from JioSaavn"
	job.UpdatedAt = time.Now().UTC()
	if err := a.update(&job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "job_storage_failed"})
		return
	}
	go a.afterAdded(job)
	writeJSON(w, http.StatusOK, job)
}

// addLossy downloads [source] to the download area, tags it, and copies it into the library
// as <artist>/<album>/<title>.<ext>, never over a file already there.
func (a *acquisitionService) addLossy(r *http.Request, source, ext, jobID string, target repairTrack, albumArtist string) (string, error) {
	dir := filepath.Join(a.cfg.libraryPath, libraryFolderName(albumArtist), libraryFolderName(target.album))
	name := libraryFolderName(target.title)
	if target.track > 1 || target.disc > 1 {
		name = fmt.Sprintf("%02d %s", target.track, name)
	}
	dest := filepath.Join(dir, name+"."+ext)
	if _, err := os.Stat(dest); err == nil {
		return "", errors.New("already_in_library")
	}
	staging := filepath.Join(a.cfg.downloadPath, "glaze", fmt.Sprintf("%s-lossy-%d-%d.%s", jobID, target.disc, target.track, ext))
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		return "", errors.New("staging_failed")
	}
	defer os.Remove(staging)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, source, nil)
	if err != nil {
		return "", errors.New("invalid_source")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return "", errors.New("source_unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", errors.New("source_unavailable")
	}
	if err := writeAll(staging, io.LimitReader(res.Body, 80<<20)); err != nil {
		return "", errors.New("download_failed")
	}
	if err := repairDownloadedMetadata(r.Context(), staging, target, albumArtist); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errors.New("library_write_failed")
	}
	in, err := os.Open(staging)
	if err != nil {
		return "", errors.New("library_write_failed")
	}
	defer in.Close()
	// Copied, not renamed: downloads and the library are often different disks.
	if err := writeAll(dest, in); err != nil {
		_ = os.Remove(dest)
		return "", errors.New("library_write_failed")
	}
	return dest, nil
}

func writeAll(path string, from io.Reader) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, from); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
