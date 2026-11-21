package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var errMetadataReview = errors.New("metadata_review_required")

type fingerprintRecording struct {
	ID, Title     string
	Duration      float64
	Artists       []struct{ Name string }
	ReleaseGroups []struct{ Title string } `json:"releasegroups"`
}
type fingerprintResult struct {
	Score      float64
	Recordings []fingerprintRecording
}
type fingerprintCache struct {
	Fetched time.Time
	Results []fingerprintResult
}
type repairTrack struct {
	title, artist, album string
	track, disc          int
	duration             int
}

func (a *acquisitionService) fingerprintLookup(ctx context.Context, filename string) ([]fingerprintResult, error) {
	if a.cfg.acoustidKey == "" {
		return nil, errors.New("fingerprinting_not_configured")
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, "fpcalc", "-json", filename)
	var output metadataOutput
	command.Stdout = &output
	if command.Run() != nil {
		return nil, errors.New("fingerprint_failed")
	}
	var fingerprint struct {
		Duration    float64
		Fingerprint string
	}
	if json.Unmarshal(output.Bytes(), &fingerprint) != nil || fingerprint.Duration <= 0 || fingerprint.Fingerprint == "" {
		return nil, errors.New("fingerprint_invalid")
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%.3f:%s", fingerprint.Duration, fingerprint.Fingerprint)))
	key := hex.EncodeToString(digest[:])
	// ponytail: serialize rare recovery lookups to respect AcoustID's rate limit.
	a.fingerprintMu.Lock()
	defer a.fingerprintMu.Unlock()
	cacheDir := filepath.Join(a.cfg.downloadPath, "glaze", "fingerprints")
	cachePath := filepath.Join(cacheDir, key+".json")
	var hit fingerprintCache
	if info, err := os.Lstat(cachePath); err == nil && info.Mode().IsRegular() && info.Size() <= 1<<20 {
		if data, err := os.ReadFile(cachePath); err == nil && json.Unmarshal(data, &hit) == nil && time.Since(hit.Fetched) < 30*24*time.Hour {
			return hit.Results, nil
		}
	}
	wait := time.Until(a.nextFingerprint)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	a.nextFingerprint = time.Now().Add(350 * time.Millisecond)
	params := url.Values{"client": {a.cfg.acoustidKey}, "duration": {fmt.Sprint(int(math.Round(fingerprint.Duration)))},
		"fingerprint": {fingerprint.Fingerprint}, "meta": {"recordings releasegroups"}}
	request, err := http.NewRequestWithContext(ctx, "POST", "https://api.acoustid.org/v2/lookup", strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, errors.New("fingerprint_service_unavailable")
	}
	defer response.Body.Close()
	var body struct {
		Status  string
		Results []fingerprintResult
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body) != nil || body.Status != "ok" {
		return nil, errors.New("fingerprint_service_unavailable")
	}
	// Bounded persistent cache survives job retries and container updates.
	if len(body.Results) > 0 && os.MkdirAll(cacheDir, 0700) == nil {
		if entries, err := os.ReadDir(cacheDir); err == nil && len(entries) >= 256 {
			var oldest os.FileInfo
			for _, entry := range entries {
				info, err := entry.Info()
				if err == nil && info.Mode().IsRegular() && strings.HasSuffix(info.Name(), ".json") && (oldest == nil || info.ModTime().Before(oldest.ModTime())) {
					oldest = info
				}
			}
			if oldest != nil {
				_ = os.Remove(filepath.Join(cacheDir, oldest.Name()))
			}
		}
		data, _ := json.Marshal(fingerprintCache{time.Now(), body.Results})
		_ = os.WriteFile(cachePath, data, 0600)
	}
	return body.Results, nil
}

func fingerprintMatches(results []fingerprintResult, target repairTrack, durationMS int64) (bool, bool) {
	matches := map[string]bool{}
	albumConfirmed := false
	for _, result := range results {
		if result.Score < .95 {
			continue
		}
		for _, recording := range result.Recordings {
			if recording.ID == "" || normalize(recording.Title) != normalize(target.title) ||
				!sameReleaseVariant(recording.Title, target.title) || recording.Duration <= 0 ||
				math.Abs(recording.Duration-float64(durationMS)/1000) > 3 ||
				(target.duration > 0 && math.Abs(float64(target.duration)-float64(durationMS)/1000) > 3) ||
				!slices.ContainsFunc(recording.Artists, func(artist struct{ Name string }) bool {
					return containsName(artist.Name, target.artist) || containsName(target.artist, artist.Name)
				}) {
				continue
			}
			matches[recording.ID] = true
			if slices.ContainsFunc(recording.ReleaseGroups, func(group struct{ Title string }) bool {
				return normalize(group.Title) == normalize(target.album) && sameReleaseVariant(group.Title, target.album)
			}) {
				albumConfirmed = true
			}
		}
	}
	return len(matches) == 1, albumConfirmed
}

// Fingerprints establish recordings, not editions. A full catalogue tracklist and
// explicit review are required before correcting conflicting album identity.
func (a *acquisitionService) recoverMetadata(ctx context.Context, job *acquisitionJob, source string, entries []os.DirEntry, metadata map[string]audioMetadata) (map[string]repairTrack, error) {
	job.CanApproveMetadata, job.ReviewSummary = false, ""
	job.ReviewFiles = nil
	review := func(reason string) (map[string]repairTrack, error) {
		job.ReviewSummary = reason
		return nil, errMetadataReview
	}
	if a.cfg.acoustidKey == "" {
		return review("The downloaded tags do not match. Configure an AcoustID application key in Companion and retry to verify the audio.")
	}
	targets := []repairTrack{}
	albumTitle := job.AlbumTitle
	if job.Kind == "album" {
		albumTitle = job.Title
		if _, err := a.jobTrackCount(ctx, job); err != nil || job.CatalogueAlbumID == "" {
			return review("A complete catalogue release is needed to recover this album safely.")
		}
		album, err := a.catalogueAlbum(ctx, job.CatalogueAlbumID)
		if err != nil || album.Count != len(entries) {
			return review("The file count does not match the requested release. Choose another download.")
		}
		for index, raw := range album.Tracks.Data {
			var track struct {
				Title         string
				Duration      int
				DiskNumber    int `json:"disk_number"`
				TrackPosition int `json:"track_position"`
				Artist        struct{ Name string }
			}
			if json.Unmarshal(raw, &track) != nil || track.Title == "" {
				return review("Catalogue metadata is incomplete.")
			}
			number, disc := track.TrackPosition, track.DiskNumber
			if number < 1 {
				number = index + 1
			}
			if disc < 1 {
				disc = 1
			}
			artist := track.Artist.Name
			if artist == "" {
				artist = job.Artist
			}
			targets = append(targets, repairTrack{track.Title, artist, albumTitle, number, disc, track.Duration})
		}
	} else {
		targets = append(targets, repairTrack{title: job.Title, artist: job.Artist, album: albumTitle, track: 1, disc: 1})
	}
	repairs := make(map[string]repairTrack)
	used := make(map[int]bool)
	editionUncertain := false
	for _, entry := range entries {
		data := metadata[entry.Name()]
		lookup := a.lookupFingerprint
		if lookup == nil {
			lookup = a.fingerprintLookup
		}
		results, err := lookup(ctx, filepath.Join(source, entry.Name()))
		if err != nil {
			return review("Audio verification failed (" + err.Error() + "). Retry after checking Companion configuration.")
		}
		matched, confirmed := -1, false
		for index, target := range targets {
			if ok, album := fingerprintMatches(results, target, data.DurationMS); ok {
				if matched >= 0 {
					return review("More than one catalogue track matches this audio. It needs manual review.")
				}
				matched, confirmed = index, album
			}
		}
		if matched < 0 || used[matched] {
			return review("The audio could not be uniquely matched to the requested recording. Choose another download.")
		}
		used[matched] = true
		target := targets[matched]
		originalAlbum := ""
		if values := data.Properties["ALBUM"]; len(values) > 0 {
			originalAlbum = values[0]
		}
		if target.album == "" {
			target.album = originalAlbum
		}
		if target.album == "" || (!confirmed && normalize(originalAlbum) != normalize(target.album)) ||
			(originalAlbum != "" && normalize(originalAlbum) != normalize(target.album)) {
			editionUncertain = true
		}
		if compilationSource(originalAlbum) || slices.Contains(data.Properties["COMPILATION"], "1") {
			return review("A compilation cannot be automatically converted into the requested release.")
		}
		repairs[entry.Name()] = target
	}
	if len(used) != len(targets) {
		return review("The downloaded audio does not cover the complete requested tracklist.")
	}
	if editionUncertain && job.ApproveMetadata {
		for _, entry := range entries {
			digest, err := taggingDigestContext(ctx, filepath.Join(source, entry.Name()))
			if err != nil || job.ApprovedFiles[entry.Name()] != digest {
				job.ApproveMetadata = false
				break
			}
		}
	}
	if editionUncertain && !job.ApproveMetadata {
		job.CanApproveMetadata = albumTitle != ""
		job.ReviewFiles = make(map[string]string)
		for _, entry := range entries {
			digest, err := taggingDigestContext(ctx, filepath.Join(source, entry.Name()))
			if err != nil {
				job.CanApproveMetadata = false
				return review("The download changed or became unreadable during review.")
			}
			job.ReviewFiles[entry.Name()] = digest
		}
		return review("All recordings match, but audio fingerprints cannot prove the album edition. Confirm that these files belong to “" + albumTitle + "” before importing with corrected tags.")
	}
	return repairs, nil
}

func repairDownloadedMetadata(ctx context.Context, filename string, target repairTrack, albumArtist string) error {
	properties := map[string][]string{"TITLE": {target.title}, "ARTIST": {target.artist}, "ALBUM": {target.album},
		"ALBUMARTIST": {albumArtist}, "TRACKNUMBER": {fmt.Sprint(target.track)}, "DISCNUMBER": {fmt.Sprint(target.disc)}}
	input, _ := json.Marshal(properties)
	repairCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(repairCtx, "glaze-taglib", "--repair-download", filename)
	command.Stdin = strings.NewReader(string(input))
	var output metadataOutput
	command.Stdout = &output
	if command.Run() != nil {
		return errors.New("download_tag_repair_failed")
	}
	var after audioMetadata
	if json.Unmarshal(output.Bytes(), &after) != nil {
		return errors.New("download_tag_repair_failed")
	}
	for key, wanted := range properties {
		if !slices.Equal(after.Properties[key], wanted) {
			return errors.New("download_tag_repair_failed")
		}
	}
	return nil
}
