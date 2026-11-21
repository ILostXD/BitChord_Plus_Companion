package main

import "testing"

func album(title, artist string) deezerAlbum {
	var a deezerAlbum
	a.Title = title
	a.Artist.Name = artist
	return a
}

func TestSameReleaseToleratesCreditsAccentsAndLabels(t *testing.T) {
	yes := []struct{ catTitle, catArtist, title, artist string }{
		{"The Blueprint 3", "JAY Z", "The Blueprint 3", "JAŸ-Z"},
		{"Rockstar Junkie", "Loe Shimmy", "Rockstar Junkie", "Loe Shimmy, Don Toliver"},
		{"GMFU", "Offset", "GMFU", "Offset & Quavo"},
		{"The Sign", "Ace of Base", "The Sign", "Ace of Base"},
		{"Take Care (Deluxe)", "Drake", "Take Care (Deluxe)", "Drake"},
		{"Born Sinner (Deluxe Version)", "J. Cole", "Born Sinner (Deluxe Version)", "J. Cole"},
	}
	for _, c := range yes {
		if !sameRelease(album(c.catTitle, c.catArtist), c.artist, c.title) {
			t.Errorf("%s / %s should match %s / %s", c.catTitle, c.catArtist, c.title, c.artist)
		}
	}
	no := []struct{ catTitle, catArtist, title, artist string }{
		{"The Sign (The Remixes)", "Ace of Base", "The Sign", "Ace of Base"},
		{"Take Care", "Drake", "Take Care (Deluxe)", "Drake"},
		{"The Sign", "Bande à Part", "The Sign", "Ace of Base"},
		{"The Blueprint", "JAY Z", "The Blueprint 3", "JAŸ-Z"},
	}
	for _, c := range no {
		if sameRelease(album(c.catTitle, c.catArtist), c.artist, c.title) {
			t.Errorf("%s / %s must not match %s / %s", c.catTitle, c.catArtist, c.title, c.artist)
		}
	}
}
