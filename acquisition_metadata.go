package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type audioMetadata struct {
	Properties map[string][]string `json:"properties"`
	DurationMS int64               `json:"duration_ms"`
}

// Receipts make both legacy imports and staged metadata repairs recoverable.
// Downloads with valid metadata retain their original bytes.
type taggingReceiptFile struct {
	OriginalSize int64  `json:"original_size"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type taggingReceipt struct {
	Version int                           `json:"version"`
	JobID   string                        `json:"job_id"`
	Files   map[string]taggingReceiptFile `json:"files"`
}

func taggingDigest(filename string) (string, error) {
	return taggingDigestContext(context.Background(), filename)
}

func taggingDigestContext(ctx context.Context, filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		_, _ = h.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readTaggingReceipt(directory string, j *acquisitionJob, wanted map[string]int64) (map[string]taggingReceiptFile, error) {
	filename := filepath.Join(directory, ".glaze-tags.json")
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // Older completed jobs remain scan-only; never retag existing albums.
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("tagging_receipt_invalid")
	}
	data, err := os.ReadFile(filename)
	var receipt taggingReceipt
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.Version != 1 || receipt.JobID != j.ID || len(receipt.Files) != len(wanted) {
		return nil, errors.New("tagging_receipt_invalid")
	}
	for name, file := range receipt.Files {
		size, ok := wanted[name]
		digest, err := hex.DecodeString(file.SHA256)
		if !ok || file.OriginalSize != size || file.Size <= 0 || err != nil || len(digest) != sha256.Size {
			return nil, errors.New("tagging_receipt_invalid")
		}
	}
	return receipt.Files, nil
}

func readAudioMetadata(ctx context.Context, filename string) (audioMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "glaze-taglib", filename)
	var output metadataOutput
	command.Stdout = &output
	if command.Run() != nil {
		return audioMetadata{}, errors.New("taglib_command_failed")
	}
	var data audioMetadata
	if json.Unmarshal(output.Bytes(), &data) != nil {
		return audioMetadata{}, errors.New("taglib_response_invalid")
	}
	return data, nil
}

type metadataOutput struct{ data bytes.Buffer }

func (b *metadataOutput) Bytes() []byte { return b.data.Bytes() }

func (b *metadataOutput) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > 4<<20 {
		return 0, errors.New("taglib_response_too_large")
	}
	return b.data.Write(p)
}

func containsName(value, wanted string) bool {
	return normalize(wanted) != "" && strings.Contains(" "+normalize(value)+" ", " "+normalize(wanted)+" ")
}

// Editions are eligibility constraints, not small score penalties that FLAC or
// a free upload slot can outweigh. Punctuation/case do not identify an edition.
func sameReleaseVariant(candidate, wanted string) bool {
	for _, word := range []string{"bonus", "deluxe", "expanded", "anniversary", "remaster", "remastered", "live", "acoustic", "instrumental", "remix", "remixes", "clean", "mono", "stereo", "sped up", "slowed"} {
		if containsName(candidate, word) != containsName(wanted, word) {
			return false
		}
	}
	return true
}

func compilationSource(value string) bool {
	for _, word := range []string{"various artists", "various artist", "va", "v a", "compilation", "compilations", "playlist", "playlists"} {
		if containsName(value, word) {
			return true
		}
	}
	return false
}

func validateDownloadMetadata(job *acquisitionJob, data audioMetadata) error {
	first := func(key string) string {
		if values := data.Properties[key]; len(values) > 0 {
			return values[0]
		}
		return ""
	}
	albumArtist := first("ALBUMARTIST")
	if compilationSource(albumArtist) && !compilationSource(job.Artist) {
		return errors.New("download_is_compilation")
	}
	if slices.Contains(data.Properties["COMPILATION"], "1") && !compilationSource(job.Artist) {
		return errors.New("download_is_compilation")
	}
	album := first("ALBUM")
	if (job.Kind == "album" && normalize(album) != normalize(job.Title)) || (job.Kind == "track" && job.AlbumTitle != "" && normalize(album) != normalize(job.AlbumTitle)) {
		return errors.New("download_release_mismatch")
	}
	if job.Kind == "track" && (normalize(first("TITLE")) != normalize(job.Title) || !sameReleaseVariant(first("TITLE"), job.Title)) {
		return errors.New("download_track_mismatch")
	}
	artists := data.Properties["ARTIST"]
	if job.Kind == "album" && albumArtist != "" {
		artists = []string{albumArtist}
	}
	if !slices.ContainsFunc(artists, func(artist string) bool { return containsName(artist, job.Artist) || containsName(job.Artist, artist) }) {
		return errors.New("download_artist_mismatch")
	}
	return nil
}
