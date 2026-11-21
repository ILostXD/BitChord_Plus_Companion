package main

import (
	"testing"
	"time"
)

func TestBaseTitle(t *testing.T) {
	same := [][2]string{
		{"Nothing Was The Same (Deluxe)", "Nothing Was the Same"},
		{"Finally Rich (Deluxe Edition)", "Finally Rich"},
		{"Hold On, We're Going Home - Remastered 2013", "Hold On, We're Going Home"},
		{"Song (feat. Someone)", "Song"},
		{"Song [Explicit]", "Song"},
		{"Album (Expanded Edition)", "Album"},
	}
	for _, c := range same {
		if baseTitle(c[0]) != baseTitle(c[1]) {
			t.Errorf("%q and %q should share a base title: %q vs %q", c[0], c[1], baseTitle(c[0]), baseTitle(c[1]))
		}
	}
	different := [][2]string{
		{"Song (Live)", "Song"},
		{"Song (Remix)", "Song"},
		{"Song (Acoustic)", "Song"},
		{"Song - Instrumental", "Song"},
	}
	for _, c := range different {
		if baseTitle(c[0]) == baseTitle(c[1]) {
			t.Errorf("%q and %q must stay different recordings", c[0], c[1])
		}
	}
}

func TestCredits(t *testing.T) {
	if !overlap(credits("Metro Boomin"), credits("Metro Boomin, Travis Scott")) {
		t.Error("a missing featured credit must still match (Blue Pill)")
	}
	if !overlap(credits("Future; Drake; Travis Scott"), credits("Drake")) {
		t.Error("any shared credit matches")
	}
	if overlap(credits("X Ambassadors"), credits("Ambassadors")) {
		t.Error("a leading X is a name, not a separator")
	}
	if !overlap(credits("Metro Boomin"), credits("Metro Boomin • Travis Scott")) {
		t.Error("Navidrome joins credits with a bullet")
	}
	if overlap(credits("Drake"), credits("Kendrick Lamar")) {
		t.Error("different artists must not match")
	}
}

func TestDecideTrack(t *testing.T) {
	lib := []libSong{
		{Title: "Hold On, We're Going Home", Album: "Nothing Was the Same", Artist: "Drake", Duration: 227},
		{Title: "Nothing's Gonna Hurt You Baby", Album: "Cigarettes After Sex", Artist: "Cigarettes After Sex", Duration: 285},
		{Title: "Song", Album: "Studio", Artist: "Band", Duration: 200},
	}
	cases := []struct {
		name, artist, title string
		duration            int
		want                string
	}{
		{"deluxe copy of a song the standard album has", "Drake", "Hold On, We're Going Home", 227, decideSkip},
		{"a couple of seconds off is the same song", "Drake", "Hold On, We're Going Home", 229, decideSkip},
		{"unknown length", "Drake", "Hold On, We're Going Home", 0, decideSkip},
		{"different cut, needs a human", "Band", "Song", 221, decideReview},
		{"live recording is a different song", "Band", "Song", 330, decideDownload},
		{"not in the library", "Drake", "Furthest Thing", 207, decideDownload},
		{"same title, other artist", "Someone Else", "Song", 200, decideDownload},
		{"extra credit on the request", "Drake, Majid Jordan", "Hold On, We're Going Home", 227, decideSkip},
	}
	for _, c := range cases {
		if got := decideTrack(c.artist, c.title, c.duration, lib).Action; got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestDecideAlbum(t *testing.T) {
	lib := []libAlbum{
		{Title: "Nothing Was the Same", Artist: "Drake", Songs: 13},
		{Title: "Finally Rich (Deluxe Edition)", Artist: "Chief Keef", Songs: 20},
		{Title: "Blue Pill", Artist: "Metro Boomin", Songs: 16},
	}
	cases := []struct {
		name, artist, title string
		want                string
	}{
		{"standard owned, deluxe asked: ask first", "Drake", "Nothing Was the Same (Deluxe)", decideReview},
		{"exactly what is owned", "Drake", "Nothing Was the Same", decideSkip},
		{"deluxe owned, standard asked: nothing to add", "Chief Keef", "Finally Rich", decideSkip},
		{"credited differently", "Metro Boomin, Travis Scott", "Blue Pill", decideSkip},
		{"same title other artist", "Someone", "Blue Pill", decideDownload},
		{"not owned", "Drake", "Views", decideDownload},
	}
	for _, c := range cases {
		if got := decideAlbum(c.artist, c.title, lib).Action; got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestStripLabelsAndRealWorldTitles(t *testing.T) {
	same := [][2]string{
		{"Random Access Memories", "Random Access Memories (Édition Studio Masters)"},
		{"DAMN. COLLECTORS EDITION.", "DAMN."},
		{"Man On The Moon: The End Of Day (Int'l Version)", "Man on the Moon: The End of Day"},
		{"Slime Language 2 (Deluxe)", "Slime Language 2"},
	}
	for _, c := range same {
		if baseTitle(c[0]) != baseTitle(c[1]) {
			t.Errorf("%q vs %q: %q != %q", c[0], c[1], baseTitle(c[0]), baseTitle(c[1]))
		}
	}
	if got := stripLabels("Finally Rich (Deluxe Edition)"); got != "Finally Rich" {
		t.Errorf("search query should lose the label, got %q", got)
	}
}

func TestRelatedEditionAsksFirst(t *testing.T) {
	lib := []libAlbum{{Title: "METRO BOOMIN PRESENTS SPIDER-MAN: ACROSS THE SPIDER-VERSE [SOUNDTRACK FROM AND INSPIRED BY THE MOTION PICTURE (METROVERSE INSTRUMENTAL EDITION)]", Artist: "Metro Boomin", Songs: 20}}
	got := decideAlbum("Metro Boomin, Swae Lee, NAV", "METRO BOOMIN PRESENTS SPIDER-MAN: ACROSS THE SPIDER-VERSE (SOUNDTRACK FROM AND INSPIRED BY THE MOTION PICTURE)", lib)
	if got.Action != decideReview {
		t.Fatalf("got %s", got.Action)
	}
}

func TestLosslessBeatsBetterMatchedLossy(t *testing.T) {
	lossy := slskFile{Filename: `shared\Cigarettes After Sex\Cigarettes After Sex\Nothing's Gonna Hurt You Baby.mp3`, Size: 9_000_000, BitRate: 320, Length: 285}
	flac := slskFile{Filename: `shared\flac\Cigarettes After Sex - Nothing's Gonna Hurt You Baby.flac`, Size: 30_000_000, Length: 285}
	responses := []slskResponse{
		{Username: "mp3-in-artist-folder", HasFreeUploadSlot: true, Files: []slskFile{lossy}},
		{Username: "loose-flac", Files: []slskFile{flac}},
	}
	user, _, _ := rankSlskd(responses, "Cigarettes After Sex", "Nothing's Gonna Hurt You Baby", "track", 0, []string{"flac", "mp3", "other"}, "")
	if user != "loose-flac" {
		t.Fatalf("lossless must win, got %s", user)
	}
	user, _, _ = rankSlskd(responses[:1], "Cigarettes After Sex", "Nothing's Gonna Hurt You Baby", "track", 0, []string{"flac", "mp3", "other"}, "")
	if user != "mp3-in-artist-folder" {
		t.Fatalf("with no lossless source the mp3 is used, got %q", user)
	}
}

func TestRepeatSuppressed(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		job  acquisitionJob
		want bool
	}{
		{"fresh failure is not retried by a repeat request", acquisitionJob{Status: "failed", Error: "no_suitable_slskd_match", UpdatedAt: now.Add(-time.Hour)}, true},
		{"old failure may be tried again", acquisitionJob{Status: "failed", Error: "no_suitable_slskd_match", UpdatedAt: now.Add(-48 * time.Hour)}, false},
		{"active job is shared", acquisitionJob{Status: "downloading", UpdatedAt: now}, true},
		{"library answer is trusted for a week", acquisitionJob{Status: "in_library", UpdatedAt: now.Add(-24 * time.Hour)}, true},
		{"but not forever", acquisitionJob{Status: "in_library", UpdatedAt: now.Add(-10 * 24 * time.Hour)}, false},
	}
	for _, c := range cases {
		if got := repeatSuppressed(&c.job, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
