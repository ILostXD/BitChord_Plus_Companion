package main

import "testing"

func TestBestQobuz(t *testing.T) {
	results := []qobuzResult{
		{Source: "qobuz", MediaType: "album", ID: "wrong", Desc: "Rocket Man by Elton John"},
		{Source: "qobuz", MediaType: "album", ID: "right", Desc: "Rocket Power by Quavo"},
		{Source: "qobuz", MediaType: "track", ID: "track", Desc: "Rocket Power by Quavo"},
	}
	if got := bestQobuz(results, "Quavo", "Rocket Power", "album"); got != "right" {
		t.Fatalf("picked %q", got)
	}
	if got := bestQobuz(results, "Quavo", "Rocket Power", "track"); got != "track" {
		t.Fatalf("picked %q", got)
	}
}

func TestProviderOrderAndOptionalQobuz(t *testing.T) {
	a := acquisitionService{cfg: config{slskdURL: "http://slskd.test", providerOrder: []string{"qobuz", "slskd"}}}
	if got := a.providers(); len(got) != 1 || got[0] != "slskd" {
		t.Fatalf("unconfigured Qobuz was enabled: %v", got)
	}
	a.cfg.qobuzUsername, a.cfg.qobuzPassword = "account", "secret"
	if got := a.providers(); len(got) != 2 || got[0] != "qobuz" || got[1] != "slskd" {
		t.Fatalf("priority not respected: %v", got)
	}
}
