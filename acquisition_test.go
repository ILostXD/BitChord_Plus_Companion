package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamRequestWithDefaultHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing JSON content type")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ok"}`))
	}))
	defer server.Close()
	a := &acquisitionService{client: server.Client()}
	var result struct{ Token string }
	status, err := a.request(context.Background(), http.MethodPost, server.URL, nil, map[string]string{"x": "y"}, &result)
	if err != nil || status != http.StatusCreated || result.Token != "ok" {
		t.Fatalf("status=%d result=%+v err=%v", status, result, err)
	}
}

func TestSlskdTransferStateIsString(t *testing.T) {
	var batch slskBatch
	if err := json.Unmarshal([]byte(`{"id":"x","transfers":[{"state":"Completed, Succeeded"}]}`), &batch); err != nil || len(batch.Transfers) != 1 || batch.Transfers[0].State != "Completed, Succeeded" {
		t.Fatalf("cannot read slskd batch state: %+v, %v", batch, err)
	}
}

func TestSlskdSearchAllowsTimeForResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v0/searches":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			var request struct {
				SearchTimeout int `json:"searchTimeout"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.SearchTimeout != 15_000 {
				t.Errorf("search timeout = %d ms, want 15000: %v", request.SearchTimeout, err)
			}
			_, _ = w.Write([]byte(`{"id":"search-1"}`))
		case r.URL.Path == "/api/v0/searches/search-1":
			_, _ = w.Write([]byte(`{"isComplete":true}`))
		case r.URL.Path == "/api/v0/searches/search-1/responses":
			_, _ = w.Write([]byte(`[{"username":"peer","hasFreeUploadSlot":true,"files":[{"filename":"Young Thug\\JEFFERY\\Song.flac","size":31457280,"length":180}]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := &acquisitionService{cfg: config{slskdURL: server.URL, formatOrder: []string{"flac", "mp3", "other"}}, client: server.Client()}
	user, files, _, err := a.searchSlskd(context.Background(), "token", "Young Thug", "Song", "track", 0, "", "")
	if err != nil || user != "peer" || len(files) != 1 {
		t.Fatalf("search user=%q files=%d err=%v", user, len(files), err)
	}
}

func TestRankSlskdRejectsPartialAlbumAndWrongTrack(t *testing.T) {
	responses := []slskResponse{
		{Username: "partial", HasFreeUploadSlot: true, Files: []slskFile{
			{Filename: `Migos\Culture II\01 - Intro.flac`, Size: 30 << 20, Length: 180},
		}},
		{Username: "wrong", HasFreeUploadSlot: true, Files: []slskFile{
			{Filename: `Another Artist\Different Album\01 - Intro.flac`, Size: 30 << 20, Length: 180},
			{Filename: `Another Artist\Different Album\02 - Song.flac`, Size: 30 << 20, Length: 180},
		}},
		{Username: "complete", HasFreeUploadSlot: false, Files: []slskFile{
			{Filename: `Migos\Culture II\01 - Intro.flac`, Size: 30 << 20, Length: 180},
			{Filename: `Migos\Culture II\02 - Song.flac`, Size: 30 << 20, Length: 180},
		}},
	}
	user, files, _ := rankSlskd(responses, "Migos", "Culture II", "album", 2, []string{"flac", "mp3", "other"}, "")
	if user != "complete" || len(files) != 2 {
		t.Fatalf("selected %q with %d files", user, len(files))
	}
	user, _, _ = rankSlskd(responses, "Migos", "Totally Different", "track", 0, []string{"flac", "mp3", "other"}, "")
	if user != "" {
		t.Fatalf("selected unrelated track from %q", user)
	}
}

func TestMoveDownloadConfinedToJob(t *testing.T) {
	root := t.TempDir()
	downloads, music := filepath.Join(root, "downloads"), filepath.Join(root, "music")
	id := "0123456789abcdef0123456789abcdef"
	source := filepath.Join(downloads, "glaze", id)
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(music, 0755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 100001)
	if err := os.WriteFile(filepath.Join(source, "song.flac"), data, 0644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(downloads, "someone-elses-download.flac")
	if err := os.WriteFile(other, data, 0644); err != nil {
		t.Fatal(err)
	}
	a := &acquisitionService{cfg: config{downloadPath: downloads, libraryPath: music}}
	j := &acquisitionJob{ID: id, Artist: "Artist", Title: "Title", Kind: "track",
		Files: []slskFile{{Filename: "song.flac", Size: int64(len(data))}}}
	if err := a.moveDownload(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(music, "Artist", "Title", "song.flac")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(music, "Artist", "Title"))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0755) {
		t.Fatalf("album directory must be readable by Navidrome: %v, %v", info, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated download was removed:", err)
	}
	if err := a.moveDownload(context.Background(), j); err != nil {
		t.Fatal("completed job should survive a retry:", err)
	}
	otherJob := *j
	otherJob.ID = "1123456789abcdef0123456789abcdef"
	if err := a.moveDownload(context.Background(), &otherJob); err == nil {
		t.Fatal("another job must not overwrite this destination")
	}
}

func TestLibraryFolderNamesAndCanceledJob(t *testing.T) {
	if !safeName("AC/DC") || libraryFolderName("AC/DC") == libraryFolderName("AC%2FDC") || strings.ContainsAny(libraryFolderName("AC/DC"), "/\\") {
		t.Fatal("artist names must be accepted and mapped to distinct safe folders")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id := "0123456789abcdef0123456789abcdef"
	a := &acquisitionService{ctx: ctx, cfg: config{downloadPath: t.TempDir(), slskdURL: "http://slskd.test", providerOrder: []string{"slskd"}}, client: http.DefaultClient,
		jobs: map[string]*acquisitionJob{id: {ID: id, Artist: "AC/DC", Title: "Title", Source: "slskd", Status: "downloading"}}}
	a.run(id)
	if a.jobs[id].Status != "downloading" {
		t.Fatal("shutdown changed resumable status")
	}
}

func TestMoveResumesInterruptedOwnedStaging(t *testing.T) {
	root := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	a := &acquisitionService{cfg: config{downloadPath: filepath.Join(root, "downloads"), libraryPath: filepath.Join(root, "music")}}
	source := filepath.Join(a.cfg.downloadPath, "glaze", id)
	staging := filepath.Join(a.cfg.libraryPath, "Artist", ".glaze-"+id)
	for _, directory := range []string{source, staging} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := make([]byte, 100001)
	if err := os.WriteFile(filepath.Join(source, "song.flac"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, ".glaze-job"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "song.flac"), data[:10], 0600); err != nil {
		t.Fatal(err)
	}
	j := &acquisitionJob{ID: id, Artist: "Artist", Title: "Title", Files: []slskFile{{Filename: "song.flac", Size: int64(len(data))}}}
	if err := a.moveDownload(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if complete, err := a.libraryCopyComplete(j); !complete || err != nil {
		t.Fatalf("copy complete=%v err=%v", complete, err)
	}
}

func TestScanFailurePreservesMovedState(t *testing.T) {
	ctx := t.Context()
	id := "0123456789abcdef0123456789abcdef"
	a := &acquisitionService{ctx: ctx, cfg: config{downloadPath: t.TempDir(), providerOrder: []string{"slskd", "qobuz"}}, client: http.DefaultClient,
		jobs: map[string]*acquisitionJob{id: {ID: id, Artist: "Artist", Title: "Title", Source: "qobuz", Status: "moved"}}}
	a.run(id)
	if a.jobs[id].Status != "moved" || a.jobs[id].Source != "qobuz" || a.jobs[id].Error == "" {
		t.Fatal("rescan failure lost the completed move")
	}
}

func TestRestoreLoadsValidJobsBeforeResuming(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &acquisitionService{ctx: ctx, cfg: config{downloadPath: t.TempDir()}, jobs: map[string]*acquisitionJob{}}
	jobsDir := filepath.Join(a.cfg.downloadPath, "glaze", "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("z", 32)} {
		data, _ := json.Marshal(acquisitionJob{ID: id, Artist: "AC/DC", Title: "Title", Status: "downloading"})
		if err := os.WriteFile(filepath.Join(jobsDir, id+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a.restore()
	if len(a.jobs) != 2 {
		t.Fatalf("restored %d jobs, want two valid IDs", len(a.jobs))
	}
}

func TestIndependentDownloadsDoNotResubmitUncertainBatch(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	a := &acquisitionService{ctx: context.Background(), cfg: config{downloadPath: t.TempDir(), slskdURL: "http://slskd.test", providerOrder: []string{"slskd"}}, client: http.DefaultClient,
		jobs: map[string]*acquisitionJob{id: {ID: id, Artist: "Artist", Title: "Title", Kind: "track", Source: "slskd", Status: "failed", Error: "slskd_enqueue_failed"}}}
	a.run(id) // Recovery fails without credentials; uncertainty must survive.
	if a.jobs[id].Error != "slskd_enqueue_failed" {
		t.Fatal("uncertain batch became safe to resubmit")
	}
	existing, err := a.enqueue("Artist", "Title", "track")
	if err != nil || existing.ID != id {
		t.Fatal("duplicate request did not reuse uncertain job")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Workers must not contact real providers during this submission test.
	a.ctx = ctx
	other, err := a.enqueue("Artist", "Other", "track")
	if err != nil || other.ID == id {
		t.Fatal("independent request was blocked by an uncertain batch")
	}
	third, err := a.enqueue("Artist", "Third", "album")
	if err != nil || third.ID == other.ID {
		t.Fatal("second active download blocked a new request")
	}
	duplicate, err := a.enqueue("Artist", "Other", "track")
	if err != nil || duplicate.ID != other.ID {
		t.Fatal("active duplicate request created another download")
	}
}

func TestConcurrentImportsEachStartTheirOwnScan(t *testing.T) {
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "startScan") && starts.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok","scanStatus":{"scanning":false}}}`))
	}))
	defer server.Close()
	a := &acquisitionService{cfg: config{navidromeURL: server.URL, navidromeUsername: "user", navidromePassword: "password"}, client: server.Client()}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- a.scanNavidrome(context.Background()) }()
	<-firstStarted
	go func() { second <- a.scanNavidrome(context.Background()) }()
	select {
	case err := <-second:
		close(releaseFirst)
		t.Fatalf("second import reused an in-progress scan: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 2 {
		t.Fatal("each import must start a scan")
	}
}

func TestDownloadMatchingRejectsCompilationsAndOtherEditions(t *testing.T) {
	file := func(name string) slskFile { return slskFile{Filename: name, Size: 30 << 20, Length: 180} }
	for _, kind := range []string{"album", "track"} {
		responses := []slskResponse{
			{Username: "compilation", HasFreeUploadSlot: true, Files: []slskFile{file(`Various Artists/Rap Caviar/Quavo - HAAVIN.flac`)}},
			{Username: "artist", Files: []slskFile{file(`Quavo/HAAVIN/01 HAAVIN.flac`)}},
		}
		user, _, _ := rankSlskd(responses, "Quavo", "HAAVIN", kind, 1, []string{"flac", "mp3", "other"}, "")
		if user != "artist" {
			t.Fatalf("%s selected compilation: %q", kind, user)
		}
	}
	responses := []slskResponse{
		{Username: "bonus", HasFreeUploadSlot: true, Files: []slskFile{file(`Freddie Gibbs/Soul Sold Separately (Bonus Edition)/01.flac`)}},
		{Username: "standard", Files: []slskFile{file(`Freddie Gibbs/2022 - Soul Sold Separately [FLAC]/01.flac`)}},
	}
	for _, tc := range []struct{ title, want string }{
		{"Soul Sold Separately", "standard"}, {"Soul Sold Separately (Bonus Edition)", "bonus"},
		{"Soul Sold Separately (Deluxe)", ""},
	} {
		user, _, _ := rankSlskd(responses, "Freddie Gibbs", tc.title, "album", 1, []string{"flac", "mp3", "other"}, "")
		if user != tc.want {
			t.Fatalf("%s: got %q want %q", tc.title, user, tc.want)
		}
	}
	if got := bestQobuz([]qobuzResult{{Source: "qobuz", MediaType: "album", ID: "wrong", Desc: "Soul Sold Separately (Bonus Edition) by Freddie Gibbs"}}, "Freddie Gibbs", "Soul Sold Separately", "album"); got != "" {
		t.Fatal("Qobuz substituted edition")
	}
	single := &acquisitionJob{Artist: "Quavo", Title: "HAAVIN", Kind: "track"}
	props := map[string][]string{"TITLE": {"HAAVIN"}, "ARTIST": {"Quavo"}, "ALBUM": {"Rap Caviar"}, "ALBUMARTIST": {"Various Artists"}}
	if err := validateDownloadMetadata(single, audioMetadata{Properties: props}); err == nil {
		t.Fatal("hidden compilation tags accepted")
	}
	props["ALBUM"], props["ALBUMARTIST"] = []string{"HAAVIN"}, []string{"Quavo"}
	single.AlbumTitle = "HAAVIN"
	if err := validateDownloadMetadata(single, audioMetadata{Properties: props}); err != nil {
		t.Fatal(err)
	}
	props["ALBUM"] = []string{"Rap Caviar"}
	if err := validateDownloadMetadata(single, audioMetadata{Properties: props}); err == nil {
		t.Fatal("different album accepted despite selected single context")
	}
	user, _, _ := rankSlskd([]slskResponse{
		{Username: "wrong-album", HasFreeUploadSlot: true, Files: []slskFile{file(`Quavo/Rap Caviar/HAAVIN.flac`)}},
		{Username: "single", Files: []slskFile{file(`Quavo/HAAVIN/HAAVIN.flac`)}},
	}, "Quavo", "HAAVIN", "track", 0, []string{"flac", "mp3", "other"}, "HAAVIN")
	if user != "single" {
		t.Fatal("selected album context lost", user)
	}
	album := &acquisitionJob{Artist: "Freddie Gibbs", Title: "Soul Sold Separately", Kind: "album"}
	if err := validateDownloadMetadata(album, audioMetadata{Properties: map[string][]string{"ALBUM": {"Soul Sold Separately (Bonus Edition)"}, "ALBUMARTIST": {"Freddie Gibbs"}}}); err == nil {
		t.Fatal("hidden bonus-edition tags accepted")
	}
}

func TestImportRejectsWrongTagsBeforePublishingAndNeverRewrites(t *testing.T) {
	root := t.TempDir()
	downloads, music := filepath.Join(root, "downloads"), filepath.Join(root, "music")
	id := strings.Repeat("a", 32)
	source := filepath.Join(downloads, "glaze", id)
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	original := []byte("original download bytes")
	if err := os.WriteFile(filepath.Join(source, "track.flac"), original, 0644); err != nil {
		t.Fatal(err)
	}
	a := &acquisitionService{cfg: config{downloadPath: downloads, libraryPath: music}, readMetadata: func(context.Context, string) (audioMetadata, error) {
		return audioMetadata{Properties: map[string][]string{"TITLE": {"HAAVIN"}, "ARTIST": {"Quavo"}, "ALBUMARTIST": {"Various Artists"}}}, nil
	}}
	job := &acquisitionJob{ID: id, Artist: "Quavo", Title: "HAAVIN", Kind: "track", Files: []slskFile{{Filename: "track.flac", Size: int64(len(original))}}}
	if err := a.moveFrom(t.Context(), job, source); err == nil {
		t.Fatal("wrong release imported")
	}
	if _, err := os.Stat(filepath.Join(music, "Quavo", "HAAVIN")); !os.IsNotExist(err) {
		t.Fatal("published rejected release")
	}
	data, err := os.ReadFile(filepath.Join(source, "track.flac"))
	if err != nil || string(data) != string(original) {
		t.Fatal("changed downloaded bytes")
	}
	a.readMetadata = func(context.Context, string) (audioMetadata, error) {
		return audioMetadata{Properties: map[string][]string{"TITLE": {"HAAVIN"}, "ARTIST": {"Quavo"}, "ALBUMARTIST": {"Quavo"}}}, nil
	}
	if err := a.moveFrom(t.Context(), job, source); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(music, "Quavo", "HAAVIN", "track.flac"))
	if err != nil || string(data) != string(original) {
		t.Fatal("rewrote accepted download")
	}
}
