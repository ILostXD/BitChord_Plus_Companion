package main

import "testing"

func TestAliasCreditMatchesLibraryAlbum(t *testing.T) {
	library := []libAlbum{{Title: "BULLY (Deluxe)", Artist: "Ye", Songs: 22}}
	if decideAlbum("Kanye West", "BULLY (Deluxe)", library).Action != decideDownload {
		t.Fatal("matched without the alias")
	}
	if !sameTitleAlbum("BULLY (Deluxe)", library) {
		t.Fatal("same title not seen")
	}
	if d := decideAlbum("Kanye West, Ye, Kanye Omari West", "BULLY (Deluxe)", library); d.Action != decideSkip {
		t.Fatalf("alias credit not matched: %+v", d)
	}
}
