package main

import (
	"context"
	"net/http"
	"testing"
)

func earlyTestService(t *testing.T) *releaseService {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // jobs are recorded, never run
	a := &acquisitionService{ctx: ctx, client: &http.Client{}, jobs: map[string]*acquisitionJob{},
		cfg: config{downloadPath: t.TempDir(), slskdURL: "http://slskd.test", providerOrder: []string{"slskd"}}}
	return &releaseService{a: a}
}

func TestPreSavedAlbumFetchesReleasedTracksEarly(t *testing.T) {
	s := earlyTestService(t)
	album := upcomingRelease{ID: "1", Title: "Album", Artist: "Artist", Tracks: []releaseTrack{
		{ID: "a", Title: "Out One", Artist: "Artist", Duration: 200, Released: true},
		{ID: "b", Title: "Out Two", Artist: "Artist", Duration: 210, Released: true},
		{ID: "c", Title: "Not Yet", Artist: "Artist", Duration: 220},
	}}
	if !s.requestReleasedTracks(&album) {
		t.Fatal("released tracks must be requested")
	}
	if len(album.EarlyJobs) != 2 || album.EarlyJobs["c"] != "" {
		t.Fatalf("only the two released tracks: %v", album.EarlyJobs)
	}
	for _, id := range album.EarlyJobs {
		j := s.a.jobs[id]
		if j == nil || j.Kind != "track" || j.AlbumTitle != "Album" {
			t.Fatalf("early job must be a track of that album: %+v", j)
		}
	}
	if s.requestReleasedTracks(&album) {
		t.Fatal("asking again must not request anything twice")
	}
	if len(s.a.jobs) != 2 {
		t.Fatalf("no duplicate jobs, got %d", len(s.a.jobs))
	}
}

func TestEarlyTracksCapPerPass(t *testing.T) {
	s := earlyTestService(t)
	album := upcomingRelease{ID: "1", Title: "Album", Artist: "Artist"}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		album.Tracks = append(album.Tracks, releaseTrack{ID: id, Title: "T " + id, Artist: "Artist", Released: true})
	}
	s.requestReleasedTracks(&album)
	if len(album.EarlyJobs) != 3 {
		t.Fatalf("at most 3 per pass, got %d", len(album.EarlyJobs))
	}
	s.requestReleasedTracks(&album)
	if len(album.EarlyJobs) != 5 {
		t.Fatalf("the rest on the next pass, got %d", len(album.EarlyJobs))
	}
}

func TestReleaseDayFinishesTrackByTrack(t *testing.T) {
	s := earlyTestService(t)
	album := upcomingRelease{ID: "1", Title: "Album", Artist: "Artist", Tracks: []releaseTrack{
		{ID: "a", Title: "Out One", Artist: "Artist", Released: true},
		{ID: "b", Title: "Out Two", Artist: "Artist", Released: true},
		{ID: "c", Title: "Came Later", Artist: "Artist", Released: true},
	}}
	s.requestReleasedTracks(&album)
	// One early job failed for a reason worth retrying.
	s.a.jobs[album.EarlyJobs["b"]].Status, s.a.jobs[album.EarlyJobs["b"]].Error = "failed", "no_suitable_slskd_match"
	delete(album.EarlyJobs, "c") // its job is still running: asking again must reuse it
	album.Tracks = append(album.Tracks, releaseTrack{ID: "d", Title: "Appeared Later", Artist: "Artist", Released: true})
	before := len(s.a.jobs)
	if !s.requestRemainingTracks(&album) {
		t.Fatal("remaining tracks must be requested")
	}
	if album.EarlyJobs["a"] == "" || album.JobID == "" {
		t.Fatalf("album must follow a job: %+v", album)
	}
	if got := len(s.a.jobs) - before; got != 2 {
		t.Fatalf("the failed one and the new one, not the finished one: %d new jobs", got)
	}
}
