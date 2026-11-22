package main

import "testing"

func TestCleanArtist(t *testing.T) {
	for in, want := range map[string]string{"Travis Scott - Topic": "Travis Scott", "Offset & Quavo": "Offset & Quavo", "JAŸ-Z": "JAŸ-Z"} {
		if got := cleanArtist(in); got != want {
			t.Errorf("cleanArtist(%q) = %q, want %q", in, got, want)
		}
	}
}

// A song nobody shares alone is found as one file of its album's folder.
func TestTrackFoundInAlbumFolder(t *testing.T) {
	responses := []slskResponse{{Username: "u", Files: []slskFile{
		{Filename: `Music\Offset - GMFU\01 GMFU.flac`, Size: 30_000_000, Length: 180},
		{Filename: `Music\Offset - GMFU\02 Other.flac`, Size: 30_000_000, Length: 180},
	}}}
	user, files, score := rankSlskd(responses, "Offset", "GMFU", "track", 0, []string{"flac"}, "GMFU")
	if score < 0 || user != "u" || len(files) != 1 || files[0].Filename != `Music\Offset - GMFU\01 GMFU.flac` {
		t.Fatalf("got %q %v %d", user, files, score)
	}
}

func TestSearchAttempts(t *testing.T) {
	texts := func(as []searchAttempt) []string {
		var out []string
		for _, a := range as {
			out = append(out, normalize(a.text))
		}
		return out
	}
	got := texts(searchAttempts("Don Toliver & Grand Theft Auto VI", "TF? (from GTAVI: The Album)", "track", "TF? (from GTAVI: The Album)", ""))
	want := map[string]bool{"don toliver grand theft auto vi tf from gtavi the album": true, "don toliver tf": true, "don toliver gtavi the album": true}
	for w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	if got := texts(searchAttempts("XXXTENTACION", "?", "album", "", "")); got[0] != "xxxtentacion questionmark" {
		t.Errorf("symbol title: %v", got)
	}
}

// "TF?" from GTA VI found as the one song of a folder named after its album.
func TestSongFromItsAlbumFolder(t *testing.T) {
	files := []slskFile{{Filename: `Qobuz2\Don Toliver\TF (from GTAVI - The Album) (2026) (Explicit)\1 TF (from GTAVI - The Album).flac`, Size: 33_700_000, Length: 162}}
	user, picked, score := rankSlskd([]slskResponse{{Username: "u", Files: files}}, "Don Toliver", "TF? (from GTAVI: The Album)", "track", 0, []string{"flac"}, "GTAVI: The Album")
	if score < 0 || user != "u" || len(picked) != 1 {
		t.Fatalf("got %q %v %d", user, picked, score)
	}
}
