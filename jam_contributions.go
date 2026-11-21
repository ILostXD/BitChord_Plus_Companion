package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
)

const jamUploadLimit int64 = 100 << 20
const jamUploadBudget int64 = 2 << 30

type jamContribution struct {
	owner, filename, contentType string
	song                         map[string]any
	cover                        []byte
	size                         int64
}

func (h *jamHub) removeContributionsLocked(s *jamSession, memberID string) {
	for id, track := range s.contributions {
		if memberID != "" && track.owner != memberID {
			continue
		}
		_ = os.Remove(track.filename)
		h.contributionBytes -= track.size
		delete(s.contributions, id)
		delete(s.searchTracks, id)
		delete(s.artwork, id)
		s.Queue = slices.DeleteFunc(s.Queue, func(item jamQueueItem) bool { return item.TrackID == id })
		if s.Playback.TrackID == id {
			s.Playback = jamPlayback{UpdatedMS: time.Now().UnixMilli()}
			for client, owner := range s.clients {
				if owner == s.HostID {
					client.enqueue(map[string]any{"type": "playback.request", "command": map[string]string{"action": "next"}})
				}
			}
		}
	}
}

// Guest libraries need not be reachable from the host server. The contributing
// phone sends one original audio file, never its library URL or credentials.
func (h *jamHub) registerContributions(mux *http.ServeMux, root context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-root.Done():
				h.mu.Lock()
				for _, session := range h.sessions {
					h.endLocked(session, "shutdown")
				}
				clear(h.sessions)
				h.mu.Unlock()
				return
			case <-ticker.C:
				h.mu.Lock()
				h.expireLocked()
				h.mu.Unlock()
			}
		}
	}()
	mux.HandleFunc("POST /api/v1/jam/sessions/{sessionId}/tracks", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		session, member := h.memberLocked(w, r, r.PathValue("sessionId"))
		if session == nil {
			h.mu.Unlock()
			return
		}
		if member.uploading || len(session.contributions) >= 32 || h.contributionBytes+jamUploadLimit > jamUploadBudget {
			h.mu.Unlock()
			jamError(w, 429, "jam_upload_limit")
			return
		}
		member.uploading = true
		h.contributionBytes += jamUploadLimit
		h.mu.Unlock()
		reservation := jamUploadLimit
		defer func() { h.mu.Lock(); member.uploading = false; h.contributionBytes -= reservation; h.mu.Unlock() }()
		r.Body = http.MaxBytesReader(w, r.Body, jamUploadLimit+(1<<20))
		reader, err := r.MultipartReader()
		if err != nil {
			jamError(w, 400, "invalid_upload")
			return
		}
		var filename string
		defer func() {
			if filename != "" {
				_ = os.Remove(filename)
			}
		}()
		var song map[string]any
		var cover []byte
		var size int64
		var audioType string
		seen := make(map[string]bool)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				jamError(w, 400, "invalid_upload")
				return
			}
			name := part.FormName()
			if seen[name] {
				jamError(w, 400, "duplicate_upload_part")
				return
			}
			seen[name] = true
			switch name {
			case "metadata":
				data, err := io.ReadAll(io.LimitReader(part, 16385))
				if err != nil || len(data) > 16384 || json.Unmarshal(data, &song) != nil || !validJamSong(song) {
					jamError(w, 400, "invalid_metadata")
					return
				}
			case "cover":
				cover, err = io.ReadAll(io.LimitReader(part, (512<<10)+1))
				if err != nil || len(cover) > 512<<10 || !slices.Contains([]string{"image/jpeg", "image/png", "image/webp"}, http.DetectContentType(cover)) {
					jamError(w, 400, "invalid_cover")
					return
				}
			case "audio":
				file, err := os.CreateTemp("", "glaze-jam-*")
				if err != nil {
					jamError(w, 503, "jam_storage_unavailable")
					return
				}
				filename = file.Name()
				prefix := make([]byte, 512)
				n, readErr := io.ReadFull(part, prefix)
				if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
					file.Close()
					jamError(w, 400, "invalid_audio")
					return
				}
				prefix = prefix[:n]
				audioType = jamAudioType(prefix)
				if audioType == "" {
					file.Close()
					jamError(w, 400, "invalid_audio")
					return
				}
				size, err = io.Copy(file, io.MultiReader(strings.NewReader(string(prefix)), io.LimitReader(part, jamUploadLimit+1)))
				closeErr := file.Close()
				if err != nil || closeErr != nil || size > jamUploadLimit {
					jamError(w, 413, "jam_audio_too_large")
					return
				}
			default:
				jamError(w, 400, "invalid_upload_part")
				return
			}
		}
		if song == nil || size == 0 {
			jamError(w, 400, "missing_audio_or_metadata")
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.sessions[session.ID] != session || session.Members[member.ID] != member || time.Now().After(session.Expires) {
			jamError(w, 403, "session_ended")
			return
		}
		id := "jam-" + jamID(16)
		song["id"] = id
		song["suffix"] = map[string]string{"audio/flac": "flac", "audio/wav": "wav", "audio/mp4": "m4a", "audio/ogg": "ogg", "audio/mpeg": "mp3"}[audioType]
		delete(song, "coverArt")
		if len(cover) > 0 {
			song["coverArt"] = id
		}
		if session.contributions == nil {
			session.contributions = make(map[string]*jamContribution)
		}
		if session.searchTracks == nil {
			session.searchTracks = make(map[string]bool)
		}
		if session.artwork == nil {
			session.artwork = make(map[string]bool)
		}
		session.contributions[id] = &jamContribution{member.ID, filename, audioType, song, cover, size}
		session.searchTracks[id], session.artwork[id] = true, len(cover) > 0
		reservation -= size
		filename = "" // The session now owns this private temporary file.
		writeJSON(w, 201, map[string]string{"track_id": id})
	})
}

func validJamSong(song map[string]any) bool {
	for key := range song {
		if !slices.Contains([]string{"title", "artist", "album", "duration", "suffix", "samplingRate", "bitRate", "isExplicit", "track", "discNumber"}, key) {
			delete(song, key)
		}
	}
	for _, key := range []string{"title", "artist", "album", "suffix"} {
		value, ok := song[key].(string)
		if !ok || len(value) > 512 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return false
		}
	}
	if song["title"] == "" {
		return false
	}
	for key, limit := range map[string]float64{"samplingRate": 768000, "bitRate": 100000, "track": 10000, "discNumber": 1000} {
		if value, exists := song[key]; exists {
			number, ok := value.(float64)
			if !ok || number < 0 || number > limit || math.Trunc(number) != number {
				return false
			}
		}
	}
	if value, exists := song["isExplicit"]; exists {
		if _, ok := value.(bool); !ok {
			return false
		}
	}
	duration, ok := song["duration"].(float64)
	return ok && duration > 0 && duration <= 7200 && math.Trunc(duration) == duration
}

func jamAudioType(data []byte) string {
	if len(data) < 12 {
		return ""
	}
	switch {
	case string(data[:4]) == "fLaC":
		return "audio/flac"
	case string(data[:4]) == "OggS":
		return "audio/ogg"
	case string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "audio/wav"
	case string(data[4:8]) == "ftyp":
		return "audio/mp4"
	case string(data[:3]) == "ID3" || data[0] == 0xff && data[1]&0xe0 == 0xe0:
		return "audio/mpeg"
	}
	return ""
}
