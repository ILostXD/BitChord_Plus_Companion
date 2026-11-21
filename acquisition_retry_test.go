package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSlskPeerFallbackUntilSuccessfulImport(t *testing.T) {
	for _, initial := range []string{"Completed, Rejected", "Queued, Remotely"} {
		t.Run(initial, func(t *testing.T) {
			id := strings.Repeat("a", 32)
			filename := `Tyler, The Creator\OKRA\01 OKRA.flac`
			data := make([]byte, 100001)
			root := t.TempDir()
			j := &acquisitionJob{ID: id, Artist: "Tyler, The Creator", Title: "OKRA", Kind: "track", Source: "slskd",
				Status: "downloading", Username: "first", BatchID: "first", Files: []slskFile{{Filename: filename, Size: int64(len(data))}}, LastProgressAt: time.Now().Add(-time.Hour)}
			cancelled := false
			var submitted []string
			a := &acquisitionService{cfg: config{downloadPath: filepath.Join(root, "downloads"), libraryPath: filepath.Join(root, "music"),
				slskdAPIKey: "key", navidromeUsername: "u", navidromePassword: "p", formatOrder: []string{"flac", "mp3", "other"}}, jobs: map[string]*acquisitionJob{}}
			source := filepath.Join(a.cfg.downloadPath, "glaze", id)
			if err := os.MkdirAll(source, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "01 OKRA.flac"), data[:1], 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/api/v0/searches":
					_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": "cached", "searchText": "Tyler, The Creator OKRA", "startedAt": time.Now(), "isComplete": true, "responseCount": 3}})
				case r.URL.Path == "/api/v0/searches/cached/responses":
					peers := []slskResponse{}
					for _, peer := range []string{"first", "second", "third"} {
						peers = append(peers, slskResponse{Username: peer, Files: []slskFile{{Filename: filename, Size: int64(len(data))}}})
					}
					_ = json.NewEncoder(w).Encode(peers)
				case r.Method == "POST" && r.URL.Path == "/api/v0/transfers/downloads/batches":
					var body struct {
						Username string
						Options  struct{ Destination string }
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if initial == "Queued, Remotely" && !cancelled {
						t.Error("submitted another peer before cancellation")
					}
					if body.Username == "first" || slices.Contains(submitted, body.Username) {
						t.Error("retried failed peer")
					}
					if body.Options.Destination != "glaze/"+id {
						t.Error("wrong destination")
					}
					submitted = append(submitted, body.Username)
					if body.Username == "third" {
						entries, _ := os.ReadDir(source)
						if len(entries) != 0 {
							t.Error("mixed old peer output")
						}
						if err := os.WriteFile(filepath.Join(source, "01 OKRA.flac"), data, 0600); err != nil {
							t.Error(err)
						}
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"batch": map[string]any{"id": body.Username}})
				case r.Method == "DELETE":
					if r.URL.Path != "/api/v0/transfers/downloads/first/transfer-first" {
						t.Error("cancelled unrelated transfer")
					}
					cancelled = true
					w.WriteHeader(http.StatusNoContent)
				case strings.HasPrefix(r.URL.Path, "/api/v0/transfers/downloads/batches/"):
					peer := strings.TrimPrefix(r.URL.Path, "/api/v0/transfers/downloads/batches/")
					state := "Completed, Rejected"
					if peer == "first" {
						state = initial
						if cancelled {
							state = "Completed, Cancelled"
						}
					}
					if peer == "third" {
						state = "Completed, Succeeded"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": peer, "username": peer, "options": map[string]any{"destination": "glaze/" + id},
						"transfers": []any{map[string]any{"id": "transfer-" + peer, "filename": filename, "state": state}}})
				case strings.Contains(r.URL.Path, "Scan"):
					_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok","scanStatus":{"scanning":false}}}`))
				default:
					t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			a.cfg.slskdURL, a.cfg.navidromeURL, a.client = server.URL, server.URL, server.Client()
			if err := a.process(t.Context(), j); err != nil {
				t.Fatal(err)
			}
			if j.Status != "rescanned" || j.Username != "third" || !slices.Equal(j.FailedPeers, []string{"first", "second"}) {
				t.Fatalf("job = %+v", j)
			}
			imported, err := os.ReadFile(filepath.Join(a.cfg.libraryPath, libraryFolderName(j.Artist), j.Title, "01 OKRA.flac"))
			if err != nil || len(imported) != len(data) {
				t.Fatalf("import: %v, %d", err, len(imported))
			}
		})
	}
}

func TestSlskProgressDistinguishesSlowAndLocalTransfers(t *testing.T) {
	id := strings.Repeat("b", 32)
	a := &acquisitionService{cfg: config{downloadPath: t.TempDir()}, jobs: map[string]*acquisitionJob{}}
	now := time.Now().UTC()
	j := &acquisitionJob{ID: id, LastProgressAt: now.Add(-time.Hour), BytesTransferred: 100}
	var batch slskBatch
	_ = json.Unmarshal([]byte(`{"transfers":[{"state":"InProgress","bytesTransferred":101},{"state":"Queued, Remotely","bytesTransferred":0}]}`), &batch)
	if err := a.checkSlskProgress(j, batch, now); err != nil {
		t.Fatal(err)
	}
	if !j.LastProgressAt.Equal(now) {
		t.Fatal("slow transfer treated as stuck")
	}
	if err := a.checkSlskProgress(j, batch, now.Add(4*time.Minute)); err == nil || err.Error() != "slskd_download_stalled" {
		t.Fatalf("stall not detected: %v", err)
	}
	batch.Transfers[0].State = "Queued, Locally"
	if err := a.checkSlskProgress(j, batch, now.Add(5*time.Minute)); err != nil {
		t.Fatal("local capacity treated as remote stall")
	}
}

func TestSlskSwitchBarrierSurvivesCancelFailureAndRestart(t *testing.T) {
	id := strings.Repeat("c", 32)
	cancelled := false
	deny := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			if deny {
				w.WriteHeader(503)
				return
			}
			cancelled = true
			w.WriteHeader(204)
			return
		}
		state := "Queued, Remotely"
		if cancelled {
			state = "Completed, Cancelled"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "batch", "username": "peer", "options": map[string]string{"destination": "glaze/" + id},
			"transfers": []any{map[string]string{"id": "transfer", "filename": "song.flac", "state": state}}})
	}))
	defer server.Close()
	a := &acquisitionService{cfg: config{slskdURL: server.URL, slskdAPIKey: "key", downloadPath: t.TempDir()}, client: server.Client(), jobs: map[string]*acquisitionJob{}}
	j := &acquisitionJob{ID: id, Artist: "Artist", Title: "Song", Status: "downloading", Source: "slskd", Username: "peer", BatchID: "batch", SwitchPeer: true, Files: []slskFile{{Filename: "song.flac", Size: 100001}}}
	if err := a.update(j); err != nil {
		t.Fatal(err)
	}
	if err := a.nextSlskPeer(t.Context(), j); err == nil || !j.SwitchPeer || j.BatchID != "batch" || len(j.FailedPeers) != 0 {
		t.Fatalf("unsafe switch: %+v, %v", j, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restored := &acquisitionService{cfg: a.cfg, client: a.client, jobs: map[string]*acquisitionJob{}, ctx: ctx}
	restored.restore()
	j = restored.jobs[id]
	if j == nil || !j.SwitchPeer {
		t.Fatal("lost restart barrier")
	}
	deny = false
	if err := restored.nextSlskPeer(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	if j.SwitchPeer || j.Status != "searching" || j.BatchID != "" || !slices.Equal(j.FailedPeers, []string{"peer"}) {
		t.Fatalf("not advanced: %+v", j)
	}
}
