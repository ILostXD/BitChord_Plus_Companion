package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func verifiedFingerprint(title, artist, album string) []fingerprintResult {
	var data []fingerprintResult
	raw := `[{"score":0.99,"recordings":[{"id":"verified-recording","title":"` + title + `","duration":180,"artists":[{"name":"` + artist + `"}],"releasegroups":[{"title":"` + album + `"}]}]}]`
	_ = json.Unmarshal([]byte(raw), &data)
	return data
}
func TestFingerprintRecoveryPreservesSourceAndRequiresEditionReview(t *testing.T) {
	source := t.TempDir()
	original := []byte("original audio with bad tags")
	if err := os.WriteFile(filepath.Join(source, "song.flac"), original, 0600); err != nil {
		t.Fatal(err)
	}
	j := &acquisitionJob{ID: strings.Repeat("a", 32), Title: "Correct Song", Artist: "Correct Artist", AlbumTitle: "Correct Album", Kind: "track", Files: []slskFile{{Filename: "song.flac", Size: int64(len(original))}}}
	data := audioMetadata{DurationMS: 180000, Properties: map[string][]string{"TITLE": {"Bad Title"}, "ARTIST": {"Bad Artist"}, "ALBUM": {"Other Album"}}}
	a := &acquisitionService{cfg: config{libraryPath: t.TempDir(), downloadPath: t.TempDir(), acoustidKey: "configured"}, readMetadata: func(context.Context, string) (audioMetadata, error) { return data, nil },
		lookupFingerprint: func(context.Context, string) ([]fingerprintResult, error) {
			return verifiedFingerprint(j.Title, j.Artist, j.AlbumTitle), nil
		}}
	if err := a.moveFrom(context.Background(), j, source); !errors.Is(err, errMetadataReview) || !j.CanApproveMetadata || j.ReviewSummary == "" {
		t.Fatalf("edition bypassed review: %v %+v", err, j)
	}
	if got, _ := os.ReadFile(filepath.Join(source, "song.flac")); string(got) != string(original) {
		t.Fatal("review mutated original download")
	}
	j.ApproveMetadata = true
	j.ApprovedFiles = j.ReviewFiles
	if err := os.WriteFile(filepath.Join(source, "song.flac"), append(append([]byte{}, original...), '!'), 0600); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(source)
	if _, err := a.recoverMetadata(context.Background(), j, source, entries, map[string]audioMetadata{"song.flac": data}); !errors.Is(err, errMetadataReview) || j.ApproveMetadata {
		t.Fatal("approval was reused for a changed download")
	}
	_ = os.WriteFile(filepath.Join(source, "song.flac"), original, 0600)
	if _, err := a.recoverMetadata(context.Background(), j, source, entries, map[string]audioMetadata{"song.flac": data}); !errors.Is(err, errMetadataReview) {
		t.Fatal("restored download bypassed review")
	}
	j.ApproveMetadata, j.ApprovedFiles = true, j.ReviewFiles
	a.repairMetadata = func(context.Context, string, repairTrack, string) error { return errors.New("repair interrupted") }
	if err := a.moveFrom(context.Background(), j, source); err == nil {
		t.Fatal("failed staged repair was imported")
	}
	if got, _ := os.ReadFile(filepath.Join(source, "song.flac")); string(got) != string(original) {
		t.Fatal("failed repair mutated original")
	}
	a.repairMetadata = func(ctx context.Context, filename string, target repairTrack, albumArtist string) error {
		if target.title != j.Title || target.album != j.AlbumTitle || albumArtist != j.Artist {
			t.Fatal("wrong canonical repair")
		}
		if got, _ := os.ReadFile(filepath.Join(source, "song.flac")); string(got) != string(original) {
			t.Fatal("original changed before promotion")
		}
		return os.WriteFile(filename, append(append([]byte{}, original...), []byte(" corrected tags")...), 0644)
	}
	if err := a.moveFrom(context.Background(), j, source); err != nil {
		t.Fatal(err)
	}
	if complete, err := a.libraryCopyComplete(j); err != nil || !complete {
		t.Fatalf("corrected copy/receipt not crash recoverable: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("original was not cleaned after successful promotion")
	}
	// Weak/wrong recordings cannot be rescued by an approval flag.
	target := repairTrack{title: j.Title, artist: j.Artist, album: j.AlbumTitle}
	for _, result := range [][]fingerprintResult{verifiedFingerprint("Other Song", j.Artist, j.AlbumTitle), verifiedFingerprint(j.Title, "Another Artist", j.AlbumTitle), {{Score: .5, Recordings: verifiedFingerprint(j.Title, j.Artist, j.AlbumTitle)[0].Recordings}}} {
		if ok, _ := fingerprintMatches(result, target, 180000); ok {
			t.Fatal("incorrect recording accepted")
		}
	}
}
func TestJamOnlyHealthDoesNotRequireDownloads(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"subsonic-response": map[string]any{"status": "ok"}})
	}))
	defer upstream.Close()
	cfg := config{navidromeURL: upstream.URL, navidromeUsername: "user", navidromePassword: "password", slskdURL: "http://unreachable.invalid", libraryPath: "/missing"}
	w := httptest.NewRecorder()
	healthHandler(cfg, time.Now())(w, httptest.NewRequest("GET", "/health", nil))
	var health healthReport
	_ = json.Unmarshal(w.Body.Bytes(), &health)
	if w.Code != 200 || health.DownloadsEnabled || len(health.Dependencies) != 1 {
		t.Fatalf("Jam-only health required slskd/mounts: %s", w.Body.String())
	}
}
