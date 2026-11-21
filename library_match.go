package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Deciding whether something asked for is already in the library, in another edition,
// or credited differently, before any download is started. Everything below the
// "Navidrome access" divider is pure and unit tested.

type libSong struct {
	ID, Title, Album, Artist string
	Artists                  []string
	Duration                 int
}

type libAlbum struct {
	ID, Title, Artist string
	Artists           []string
	Songs             int
}

// Decision outcomes for a request.
const (
	decideDownload = "download"
	decideSkip     = "skip"
	decideReview   = "review"
)

type libraryDecision struct {
	Action  string
	Summary string // for the dashboard: what is already there, or what differs
}

var (
	bracketed = regexp.MustCompile(`[\(\[][^\)\]]*[\)\]]`)
	dashTail  = regexp.MustCompile(`\s+[-–—]\s+[^-–—]*$`)
	creditSep = regexp.MustCompile(`(?i)\s*(?:,|;|&|•|·)\s*|\s+(?:feat\.?|ft\.?|featuring|with|x)\s+`)
)

// Words that make a bracket or a " - suffix" an edition/credit label rather than part of the name.
var editionWords = []string{
	"feat", "ft.", "featuring", "with ", "deluxe", "expanded", "remaster", "explicit", "clean",
	"edition", "bonus", "anniversary", "super", "collector", "special", "platinum", "version",
	"mono", "stereo", "single", "album version", "radio edit", "studio master", "master", "metroverse", "int'l", "international",
}

// Words that make something a different recording even when it is bracketed.
var recordingWords = []string{"live", "acoustic", "remix", "instrumental", "demo", "karaoke", "sped up", "slowed", "a cappella", "acapella"}

func isEditionLabel(label string) bool {
	l := strings.ToLower(label)
	for _, w := range recordingWords {
		if strings.Contains(l, w) {
			return false
		}
	}
	for _, w := range editionWords {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// baseTitle is a title with edition and credit labels removed, normalised for comparison:
// "Nothing Was The Same (Deluxe)" and "Nothing Was the Same" are the same base title,
// "Song (feat. X)" and "Song" too, but "Song (Live)" and "Song (Remix)" are not.
func baseTitle(s string) string { return normalize(stripLabels(s)) }

var trailingEdition = regexp.MustCompile(`(?i)\s+(?:collector'?s|collectors|deluxe|expanded|special|anniversary|international|int'l|extended|platinum|standard)(?:\s+(?:edition|version))?\.?$`)

var accentFold = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ä", "a", "ã", "a", "å", "a", "ç", "c", "è", "e", "é", "e", "ê", "e", "ë", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i", "ñ", "n", "ò", "o", "ó", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "ý", "y", "ÿ", "y",
	"À", "A", "Á", "A", "Â", "A", "Ä", "A", "Ç", "C", "È", "E", "É", "E", "Ê", "E", "Ë", "E", "Í", "I", "Ï", "I",
	"Ñ", "N", "Ó", "O", "Ö", "O", "Ú", "U", "Ü", "U", "Ÿ", "Y",
)

// stripLabels removes edition and credit labels from a title but keeps its own spelling,
// so it can also be used as a search query.
func stripLabels(s string) string {
	s = accentFold.Replace(s)
	s = bracketed.ReplaceAllStringFunc(s, func(m string) string {
		if isEditionLabel(m[1 : len(m)-1]) {
			return " "
		}
		return m
	})
	if loc := dashTail.FindStringIndex(s); loc != nil && isEditionLabel(s[loc[0]:]) {
		s = s[:loc[0]]
	}
	for {
		trimmed := trailingEdition.ReplaceAllString(strings.TrimSpace(s), "")
		if trimmed == strings.TrimSpace(s) {
			return strings.TrimSpace(s)
		}
		s = trimmed
	}
}

// isDeluxe reports whether a title says it is an extended edition.
func isDeluxe(s string) bool {
	l := strings.ToLower(s)
	for _, w := range []string{"deluxe", "expanded", "special edition", "collector", "super deluxe", "bonus", "anniversary", "platinum"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// credits splits an artist credit into its normalised names.
func credits(parts ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		for _, name := range creditSep.Split(p, -1) {
			if n := normalize(name); n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func fmtDuration(sec int) string { return fmt.Sprintf("%d:%02d", sec/60, sec%60) }

// decideTrack: is [title] by [artist] (about [duration] seconds, 0 if unknown) already in [library]?
func decideTrack(artist, title string, duration int, library []libSong) libraryDecision {
	want, wantArtists := baseTitle(title), credits(artist)
	var matches []libSong
	for _, s := range library {
		if baseTitle(s.Title) == want && overlap(wantArtists, credits(append([]string{s.Artist}, s.Artists...)...)) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return libraryDecision{Action: decideDownload}
	}
	if duration <= 0 {
		return libraryDecision{decideSkip, fmt.Sprintf("Already in your library on “%s”", matches[0].Album)}
	}
	closest, gap := matches[0], math.MaxInt
	for _, m := range matches {
		if m.Duration <= 0 {
			return libraryDecision{decideSkip, fmt.Sprintf("Already in your library on “%s”", m.Album)}
		}
		if d := int(math.Abs(float64(m.Duration - duration))); d < gap {
			closest, gap = m, d
		}
	}
	switch {
	case gap <= 4:
		return libraryDecision{decideSkip, fmt.Sprintf("Already in your library on “%s”", closest.Album)}
	case gap <= 30:
		return libraryDecision{decideReview, fmt.Sprintf("You have a different cut of this song on “%s” (%s, this one is %s)",
			closest.Album, fmtDuration(closest.Duration), fmtDuration(duration))}
	default:
		return libraryDecision{Action: decideDownload} // a different recording that shares a name
	}
}

// decideAlbum: is the album [title] by [artist] already in [library], in whichever edition?
func decideAlbum(artist, title string, library []libAlbum) libraryDecision {
	want, wantArtists := baseTitle(title), credits(artist)
	for _, al := range library {
		if baseTitle(al.Title) != want || !overlap(wantArtists, credits(append([]string{al.Artist}, al.Artists...)...)) {
			continue
		}
		if isDeluxe(title) && !isDeluxe(al.Title) {
			return libraryDecision{decideReview, fmt.Sprintf("You have “%s” (%d songs). This is the deluxe edition, which has more songs", al.Title, al.Songs)}
		}
		return libraryDecision{decideSkip, fmt.Sprintf("Already in your library as “%s” (%d songs)", al.Title, al.Songs)}
	}
	// A related edition (one title is the other plus more words, e.g. an instrumental edition):
	// not the same album, but not worth downloading blindly either.
	for _, al := range library {
		other := baseTitle(al.Title)
		if !overlap(wantArtists, credits(append([]string{al.Artist}, al.Artists...)...)) || len(want) < 8 || len(other) < 8 {
			continue
		}
		if strings.HasPrefix(other, want+" ") || strings.HasPrefix(want, other+" ") {
			return libraryDecision{decideReview, fmt.Sprintf("You have a related edition: “%s” (%d songs)", al.Title, al.Songs)}
		}
	}
	return libraryDecision{Action: decideDownload}
}

// ---- Navidrome access ----------------------------------------------------

func (a *acquisitionService) librarySearch(ctx context.Context, query string, songs, albums int) ([]libSong, []libAlbum, error) {
	if a.cfg.navidromeURL == "" {
		return nil, nil, fmt.Errorf("library_unavailable")
	}
	var endpoint string
	if id, ok := ctx.Value(navidromeIdentityKey{}).(navidromeIdentity); ok && id.User != "" {
		endpoint = id.endpoint(a.cfg, "search3")
	} else if a.cfg.navidromeUsername != "" && a.cfg.navidromePassword != "" {
		salt := fmt.Sprintf("%x", time.Now().UnixNano())
		token := md5.Sum([]byte(a.cfg.navidromePassword + salt))
		endpoint = a.cfg.navidromeURL + "/rest/search3.view?" + url.Values{"u": {a.cfg.navidromeUsername}, "s": {salt},
			"t": {hex.EncodeToString(token[:])}, "v": {"1.16.1"}, "c": {"BitChordPlusCompanion"}, "f": {"json"}}.Encode()
	} else {
		return nil, nil, fmt.Errorf("library_unavailable")
	}
	endpoint += "&" + url.Values{"query": {query}, "songCount": {fmt.Sprint(songs)}, "albumCount": {fmt.Sprint(albums)}, "artistCount": {"0"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("library_unavailable")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil || res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("library_unavailable")
	}
	var doc struct {
		Subsonic struct {
			Status string `json:"status"`
			Result struct {
				Song []struct {
					ID, Title, Album, Artist string
					Duration                 int
					Artists                  []struct{ Name string }
				} `json:"song"`
				Album []struct {
					ID, Name, Title, Artist string
					SongCount               int
					Artists                 []struct{ Name string }
				} `json:"album"`
			} `json:"searchResult3"`
		} `json:"subsonic-response"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Subsonic.Status != "ok" {
		return nil, nil, fmt.Errorf("library_unavailable")
	}
	var outSongs []libSong
	for _, s := range doc.Subsonic.Result.Song {
		ls := libSong{ID: s.ID, Title: s.Title, Album: s.Album, Artist: s.Artist, Duration: s.Duration}
		for _, ar := range s.Artists {
			ls.Artists = append(ls.Artists, ar.Name)
		}
		outSongs = append(outSongs, ls)
	}
	var outAlbums []libAlbum
	for _, al := range doc.Subsonic.Result.Album {
		la := libAlbum{ID: al.ID, Title: al.Name, Artist: al.Artist, Songs: al.SongCount}
		if la.Title == "" {
			la.Title = al.Title
		}
		for _, ar := range al.Artists {
			la.Artists = append(la.Artists, ar.Name)
		}
		outAlbums = append(outAlbums, la)
	}
	return outSongs, outAlbums, nil
}

// libraryDecision asks the library and decides. When the library can't be read it says
// "download": a broken check must never silently block a request.
func (a *acquisitionService) decideLibrary(ctx context.Context, artist, title, kind, album string, duration int) libraryDecision {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if kind == "album" {
		query := stripLabels(title)
		songs, albums, err := a.librarySearch(ctx, query, 60, 40)
		if err != nil {
			return libraryDecision{Action: decideDownload}
		}
		if d := decideAlbum(artist, title, albums); d.Action != decideDownload {
			return d
		}
		// Asked for as an album but really a single: fine if the library has that song.
		if d := decideTrack(artist, title, 0, songs); d.Action == decideSkip {
			return d
		}
		return libraryDecision{Action: decideDownload}
	}
	songs, _, err := a.librarySearch(ctx, stripLabels(title), 60, 0)
	if err != nil {
		return libraryDecision{Action: decideDownload}
	}
	return decideTrack(artist, title, duration, songs)
}

// recordLibraryDecision answers a request the library already covers (or can't decide
// alone) with a job that says so, instead of starting a download. It returns nil when the
// request should go ahead. A repeat of a request already recorded returns the record.
func (a *acquisitionService) recordLibraryDecision(ctx context.Context, artist, title, kind, album string, duration int) *acquisitionJob {
	return a.recordLibraryDecisionOpt(ctx, artist, title, kind, album, duration, false)
}

// recordLibraryDecisionOpt: [retry] lets a failed earlier request be asked for again.
func (a *acquisitionService) recordLibraryDecisionOpt(ctx context.Context, artist, title, kind, album string, duration int, retry bool) *acquisitionJob {
	a.mu.Lock()
	for _, existing := range a.jobs {
		if strings.EqualFold(existing.Artist, artist) && strings.EqualFold(existing.Title, title) && existing.Kind == kind &&
			normalize(existing.AlbumTitle) == normalize(album) && repeatSuppressed(existing, time.Now()) && !(retry && existing.Status == "failed") {
			snapshot := *existing
			a.mu.Unlock()
			return &snapshot
		}
	}
	a.mu.Unlock()
	d := a.decideLibrary(ctx, artist, title, kind, album, duration)
	if d.Action == decideDownload {
		return nil
	}
	status, reason := "in_library", ""
	if d.Action == decideReview {
		status, reason = "needs_review", "library_review"
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil
	}
	now := time.Now().UTC()
	j := &acquisitionJob{ID: hex.EncodeToString(idBytes), Artist: artist, Title: title, Kind: kind, AlbumTitle: album,
		Source: "library", Status: status, Error: reason, ReviewSummary: d.Summary, CreatedAt: now, UpdatedAt: now}
	if a.update(j) != nil {
		return nil
	}
	snapshot := *j
	return &snapshot
}

// coveredByLibrary: when the library already has what a failed job asked for (or needs a
// decision about it), the job becomes that answer. It returns nil when a retry should go ahead.
func (a *acquisitionService) coveredByLibrary(ctx context.Context, saved acquisitionJob) *acquisitionJob {
	d := a.decideLibrary(ctx, saved.Artist, saved.Title, saved.Kind, saved.AlbumTitle, 0)
	if d.Action == decideDownload {
		return nil
	}
	j := saved
	j.UpdatedAt = time.Now().UTC()
	j.Source, j.ReviewSummary = "library", d.Summary
	j.Status, j.Error = "in_library", ""
	if d.Action == decideReview {
		j.Status, j.Error = "needs_review", "library_review"
	}
	if a.update(&j) != nil {
		return nil
	}
	return &j
}

// sweepFailed re-checks old failures against the library once at start: a request that
// failed before this check existed is often for something the library already has.
func (a *acquisitionService) sweepFailed() {
	time.Sleep(20 * time.Second)
	a.mu.Lock()
	var failed []acquisitionJob
	for _, j := range a.jobs {
		if j.Status == "failed" {
			failed = append(failed, *j)
		}
	}
	a.mu.Unlock()
	checked := map[string]string{}
	for _, j := range failed {
		if a.ctx.Err() != nil {
			return
		}
		key := strings.ToLower(j.Artist + "|" + j.Title + "|" + j.Kind)
		if action, seen := checked[key]; seen {
			// A repeat of one already looked at: when the library covers it, the older copy just closes.
			if action != decideDownload {
				j.Status, j.Error, j.Source, j.UpdatedAt = "skipped", "", "library", time.Now().UTC()
				_ = a.update(&j)
			}
			continue
		}
		d := a.decideLibrary(a.ctx, j.Artist, j.Title, j.Kind, j.AlbumTitle, 0)
		checked[key] = d.Action
		if d.Action != decideDownload {
			j.Status, j.Error, j.Source, j.ReviewSummary = "in_library", "", "library", d.Summary
			if d.Action == decideReview {
				j.Status, j.Error = "needs_review", "library_review"
			}
			j.UpdatedAt = time.Now().UTC()
			_ = a.update(&j)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
