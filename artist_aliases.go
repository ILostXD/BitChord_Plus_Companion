package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

var aliasCache sync.Map // normalized name -> []string

// artistAliases: every name MusicBrainz knows [name]'s artist by ("Kanye West" -> "Ye",
// "Kanye Omari West", ...), so a library that files an album under one of them still has it.
// Only artists whose own name or alias is [name] count; nil when MusicBrainz can't say.
func (a *acquisitionService) artistAliases(ctx context.Context, name string) []string {
	key := normalize(name)
	if key == "" {
		return nil
	}
	if cached, ok := aliasCache.Load(key); ok {
		return cached.([]string)
	}
	var found struct {
		Artists []struct {
			Name    string `json:"name"`
			Score   int    `json:"score"`
			Aliases []struct {
				Name string `json:"name"`
			} `json:"aliases"`
		} `json:"artists"`
	}
	q := fmt.Sprintf("artist:\"%s\" OR alias:\"%s\"", name, name)
	if err := a.musicBrainz(ctx, "artist/?"+url.Values{"query": {q}, "fmt": {"json"}, "limit": {"5"}}.Encode(), &found); err != nil {
		return nil
	}
	var out []string
	for _, artist := range found.Artists {
		names := []string{artist.Name}
		for _, alias := range artist.Aliases {
			names = append(names, alias.Name)
		}
		match := false
		for _, n := range names {
			if normalize(n) == key {
				match = true
			}
		}
		if match && artist.Score >= 90 {
			out = append(out, names...)
		}
	}
	aliasCache.Store(key, out)
	return out
}

// withAliases: [artist] followed by the other names of its credited artists, as one credit line.
func (a *acquisitionService) withAliases(ctx context.Context, artist string) string {
	names := []string{artist}
	for _, credit := range credits(artist) {
		names = append(names, a.artistAliases(ctx, credit)...)
	}
	return strings.Join(names, ", ")
}
