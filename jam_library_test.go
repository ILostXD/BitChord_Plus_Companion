package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestJamLibraryStreamsHostAndGuestContributions(t *testing.T) {
	hostToken := strings.Repeat("a", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("u") != "host-account" || r.URL.Query().Get("t") != hostToken {
			t.Error("member was not authenticated as the host")
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/rest/getSong.view", "/rest/search3.view":
			song := map[string]any{"id": "host-song", "title": "Host Song", "artist": "Host Artist", "album": "Host Album", "duration": 180, "coverArt": "mf-host-song", "albumId": "album", "path": "/private/music/file.flac", "userRating": 5}
			data := map[string]any{"status": "ok", "song": song}
			if strings.Contains(r.URL.Path, "search3") {
				data = map[string]any{"status": "ok", "searchResult3": map[string]any{"song": []any{song}}}
			}
			writeJSON(w, 200, map[string]any{"subsonic-response": data})
		case "/rest/getCoverArt.view":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("image"))
		case "/rest/stream.view":
			if r.Header.Get("Range") != "bytes=4-7" || r.URL.Query().Get("format") != "raw" {
				t.Error("range/raw audio not forwarded")
			}
			w.Header().Set("Content-Type", "audio/flac")
			w.Header().Set("Content-Range", "bytes 4-7/12")
			w.WriteHeader(206)
			_, _ = w.Write([]byte("data"))
		default:
			writeJSON(w, 200, map[string]any{"subsonic-response": map[string]any{"status": "ok"}})
		}
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := &jamMember{ID: jamID(12), token: jamID(32), Name: "Host"}
	guest := &jamMember{ID: jamID(12), token: jamID(32), Name: "Guest from another server"}
	s := &jamSession{ID: jamID(16), HostID: host.ID, Expires: time.Now().Add(time.Hour), Members: map[string]*jamMember{host.ID: host, guest.ID: guest},
		Playback: jamPlayback{TrackID: "host-song"}, Queue: []jamQueueItem{}, clients: map[*jamClient]string{},
		libraryIdentity: navidromeIdentity{"host-account", strings.Repeat("b", 24), hostToken}, artwork: map[string]bool{}, searchTracks: map[string]bool{}}
	h := &jamHub{sessions: map[string]*jamSession{s.ID: s}, cfg: config{navidromeURL: upstream.URL}}
	mux := http.NewServeMux()
	h.registerREST(mux)
	h.registerLibrary(mux, ctx)
	h.registerContributions(mux, ctx)
	base := "/api/v1/jam/sessions/" + s.ID
	read := func(member *jamMember, endpoint, id string) *httptest.ResponseRecorder {
		salt := strings.Repeat("c", 24)
		sum := md5.Sum([]byte(member.token + salt))
		params := url.Values{"u": {member.ID}, "s": {salt}, "t": {hex.EncodeToString(sum[:])}, "id": {id}, "query": {"h"}}
		r := httptest.NewRequest("GET", base+"/library/rest/"+endpoint+".view?"+params.Encode(), nil)
		r.Header.Set("Range", "bytes=4-7")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	metadata := read(guest, "getSong", "host-song")
	if metadata.Code != 200 || strings.Contains(metadata.Body.String(), "/private/") || strings.Contains(metadata.Body.String(), hostToken) || strings.Contains(metadata.Body.String(), "userRating") {
		t.Fatalf("unsafe metadata: %d %s", metadata.Code, metadata.Body.String())
	}
	if read(guest, "getCoverArt", "al-album").Code != 200 || read(guest, "stream", "host-song").Code != 206 {
		t.Fatal("host art/audio unavailable to different-library guest")
	}
	if read(guest, "stream", "not-in-queue").Code != 403 || read(guest, "setRating", "host-song").Code != 403 {
		t.Fatal("invitation granted unrelated/write access")
	}
	stranger := &jamMember{ID: guest.ID, token: jamID(32)}
	if read(stranger, "getSong", "host-song").Code != 403 {
		t.Fatal("wrong secret accepted")
	}
	if read(guest, "search3", "").Code != 200 {
		t.Fatal("one-letter host search unavailable")
	}

	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	field, _ := writer.CreateFormField("metadata")
	_, _ = io.WriteString(field, `{"title":"Guest Song","artist":"Guest Artist","album":"Guest Album","suffix":"flac","duration":180,"path":"/guest/private"}`)
	audio, _ := writer.CreateFormFile("audio", "ignored.flac")
	_, _ = io.WriteString(audio, "fLaCguest-audio-from-different-library")
	_ = writer.Close()
	r := httptest.NewRequest("POST", base+"/tracks", &upload)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("X-Jam-Member-Token", guest.token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var contributed struct {
		TrackID string `json:"track_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &contributed)
	if w.Code != 201 || contributed.TrackID == "" {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	h.mu.Lock()
	err := h.commandLocked(s, guest, jamCommand{Type: "queue.add", TrackID: contributed.TrackID})
	h.mu.Unlock()
	if err != "" {
		t.Fatal(err)
	}
	file := s.contributions[contributed.TrackID].filename
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	remote := read(host, "getSong", contributed.TrackID)
	if remote.Code != 200 || !strings.Contains(remote.Body.String(), "Guest Song") || strings.Contains(remote.Body.String(), "/guest/private") {
		t.Fatal("contribution metadata was not shared safely")
	}
	stream := read(host, "stream", contributed.TrackID)
	if stream.Code != 206 || stream.Body.String() != "gues" {
		t.Fatalf("guest audio seek: %d %q", stream.Code, stream.Body.String())
	}
	r = httptest.NewRequest("POST", base+"/leave", nil)
	r.Header.Set("X-Jam-Member-Token", guest.token)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || read(guest, "getSong", "host-song").Code != 403 {
		t.Fatal("departed member retained access")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) || h.contributionBytes != 0 {
		t.Fatal("contributed audio not cleaned after leave")
	}
	if len(s.Queue) != 0 {
		t.Fatal("unavailable contribution retained in queue")
	}
}
