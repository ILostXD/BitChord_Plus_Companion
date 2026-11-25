package main

import (
	"testing"
	"time"
)

func TestNextAutoRetry(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := nextAutoRetry(first, first.Add(time.Hour)); !got.Equal(first.Add(7 * time.Hour)) {
		t.Fatal("early failures retry after 6 hours, got", got)
	}
	if got := nextAutoRetry(first, first.Add(72*time.Hour)); !got.Equal(first.Add(96 * time.Hour)) {
		t.Fatal("later failures retry daily, got", got)
	}
	if got := nextAutoRetry(first, first.Add(29*24*time.Hour+time.Hour)); !got.IsZero() {
		t.Fatal("retries stop after 30 days, got", got)
	}
}

func TestRetryPlanOnlyNewestRetryableFailure(t *testing.T) {
	now := time.Now().UTC()
	a := &acquisitionService{jobs: map[string]*acquisitionJob{
		"old":   {ID: "old", Artist: "A - Topic", Title: "Song", Kind: "track", Status: "failed", Error: "no_suitable_slskd_match", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour)},
		"new":   {ID: "new", Artist: "A", Title: "Song", Kind: "track", Status: "failed", Error: "no_suitable_slskd_match", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		"bad":   {ID: "bad", Artist: "B", Title: "X", Kind: "album", Status: "failed", Error: "catalogue_album_mismatch", CreatedAt: now, UpdatedAt: now},
		"hid":   {ID: "hid", Artist: "C", Title: "Y", Kind: "track", Status: "failed", Error: "no_suitable_slskd_match", CreatedAt: now, UpdatedAt: now, Hidden: true},
		"done1": {ID: "done1", Artist: "D", Title: "Z", Kind: "track", Status: "failed", Error: "no_suitable_slskd_match", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)},
		"503":   {ID: "503", Artist: "E", Title: "W", Kind: "album", Status: "failed", Error: "catalogue_tracklist_unavailable: upstream_http_503", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		"done2": {ID: "done2", Artist: "D", Title: "Z", Kind: "track", Status: "rescanned", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
	}}
	plan := a.retryPlan()
	if len(plan) != 2 {
		t.Fatalf("want the newest retryable failures (new, 503), got %v", plan)
	}
	if at, ok := plan["new"]; !ok || !at.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("want new at +5h (6h after it failed), got %v", plan)
	}
}
