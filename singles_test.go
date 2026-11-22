package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReplaceSinglesMovesThemAndRepointsPlaylists(t *testing.T) {
	library := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(library, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	write("Offset/GMFU/01 GMFU.flac")
	write("Offset/GMFU/02 Bodies.flac")
	write("Offset/Bodies/01 Bodies.flac") // the single
	write("Offset/Bodies/cover.jpg")
	write("Other/Bodies/01 Bodies.flac") // same title, another artist: kept

	var mu sync.Mutex
	var rewritten []string
	nd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		reply := func(body string) { _, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok",` + body + `}}`)) }
		switch strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/rest/"), ".view") {
		case "search3":
			if q.Get("albumCount") == "20" {
				reply(`"searchResult3":{"album":[{"id":"al","name":"GMFU","artist":"Offset","songCount":2}]}`)
			} else if strings.EqualFold(q.Get("query"), "Bodies") {
				reply(`"searchResult3":{"song":[
					{"id":"a2","title":"Bodies","artist":"Offset","album":"GMFU","albumId":"al","path":"Offset/GMFU/02 Bodies.flac"},
					{"id":"s1","title":"Bodies","artist":"Offset","album":"Bodies","albumId":"sg","path":"Offset/Bodies/01 Bodies.flac"},
					{"id":"x1","title":"Bodies","artist":"Someone Else","album":"Bodies","albumId":"xg","path":"Other/Bodies/01 Bodies.flac"}]}`)
			} else {
				reply(`"searchResult3":{"song":[]}`)
			}
		case "getAlbum":
			switch q.Get("id") {
			case "al":
				reply(`"album":{"id":"al","name":"GMFU","artist":"Offset","songCount":2,"song":[
					{"id":"a1","title":"GMFU","artist":"Offset","albumId":"al"},{"id":"a2","title":"Bodies","artist":"Offset","albumId":"al"}]}`)
			default:
				reply(`"album":{"id":"` + q.Get("id") + `","name":"Bodies","songCount":1}`)
			}
		case "getPlaylists":
			reply(`"playlists":{"playlist":[{"id":"p1"},{"id":"p2"}]}`)
		case "getPlaylist":
			if q.Get("id") == "p1" {
				reply(`"playlist":{"entry":[{"id":"z"},{"id":"s1"},{"id":"y"}]}`)
			} else {
				reply(`"playlist":{"entry":[{"id":"z"}]}`)
			}
		case "createPlaylist":
			mu.Lock()
			rewritten = append([]string{q.Get("playlistId")}, q["songId"]...)
			mu.Unlock()
			reply(`"x":{}`)
		case "startScan", "getScanStatus":
			reply(`"scanStatus":{"scanning":false}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer nd.Close()
	a := &acquisitionService{cfg: config{libraryPath: library, navidromeURL: nd.URL, navidromeUsername: "u", navidromePassword: "p"},
		client: nd.Client(), jobs: map[string]*acquisitionJob{}}
	n, err := a.replaceSingles(context.Background(), acquisitionJob{Artist: "Offset", Title: "GMFU", Kind: "album"})
	if err != nil || n != 1 {
		t.Fatalf("replaced %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(library, "Offset/Bodies/01 Bodies.flac")); !os.IsNotExist(err) {
		t.Fatal("single still in the library")
	}
	if _, err := os.Stat(filepath.Join(library, "Offset/Bodies")); !os.IsNotExist(err) {
		t.Fatal("emptied single folder left behind")
	}
	matches, _ := filepath.Glob(filepath.Join(library, ".replaced-singles", "*", "Offset", "Bodies", "*"))
	if len(matches) != 2 {
		t.Fatalf("single and its cover not kept aside: %v", matches)
	}
	for _, kept := range []string{"Offset/GMFU/02 Bodies.flac", "Other/Bodies/01 Bodies.flac"} {
		if _, err := os.Stat(filepath.Join(library, kept)); err != nil {
			t.Fatalf("%s was touched", kept)
		}
	}
	got, _ := json.Marshal(rewritten)
	if string(got) != `["p1","z","a2","y"]` {
		t.Fatalf("playlist rewritten as %s", got)
	}
}
