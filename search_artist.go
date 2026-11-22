package main

import (
	"regexp"
	"strings"
	"unicode"
)

// YouTube auto-generated channels are named "<Artist> - Topic"; requests made before the app
// sent album artists can still carry one, and it is not part of the artist.
var topicSuffix = regexp.MustCompile(`(?i)\s*-\s*topic\s*$`)

func cleanArtist(artist string) string {
	return strings.TrimSpace(topicSuffix.ReplaceAllString(strings.TrimSpace(artist), ""))
}

// searchText keeps a query's letters and digits as written (accents included, as folder names
// have them) and drops punctuation no folder name carries ("TF? (from GTAVI: The Album)").
func searchText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '$' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
