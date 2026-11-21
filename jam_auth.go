package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var jamHex = regexp.MustCompile(`^[0-9a-f]+$`)

type navidromeIdentity struct{ User, Salt, Token string }
type navidromeIdentityKey struct{}

func navidromeRequestIdentity(r *http.Request, cfg config) (navidromeIdentity, bool) {
	a := navidromeIdentity{r.Header.Get("X-Jam-Navidrome-User"), r.Header.Get("X-Jam-Navidrome-Salt"), r.Header.Get("X-Jam-Navidrome-Token")}
	return a, a.User != "" && len(a.User) <= 128 && len(a.Salt) == 24 && len(a.Token) == 32 &&
		jamHex.MatchString(a.Salt) && jamHex.MatchString(a.Token) && cfg.navidromeURL != ""
}

func (a navidromeIdentity) endpoint(cfg config, route string) string {
	params := url.Values{"u": {a.User}, "s": {a.Salt}, "t": {a.Token},
		"v": {"1.16.1"}, "c": {"BitChordPlusCompanion"}, "f": {"json"}}
	if route == "getUser" {
		params.Set("username", a.User)
	}
	return cfg.navidromeURL + "/rest/" + route + ".view?" + params.Encode()
}

// Resolve the canonical account name as well as validating the token. Credentials
// live only in the request context; they are never kept in release storage.
func authenticateCompanionUser(r *http.Request, cfg config, client *http.Client) (navidromeIdentity, bool) {
	a, valid := navidromeRequestIdentity(r, cfg)
	if !valid {
		return a, false
	}
	result, body := fetch(r.Context(), client, http.MethodGet, a.endpoint(cfg, "getUser"), nil, nil)
	var response struct {
		Subsonic struct {
			Status string
			User   struct{ Username string }
		} `json:"subsonic-response"`
	}
	if result.HTTPStatus != http.StatusOK || result.Error != "" || json.Unmarshal(body, &response) != nil ||
		response.Subsonic.Status != "ok" || !strings.EqualFold(response.Subsonic.User.Username, a.User) {
		return a, false
	}
	a.User = response.Subsonic.User.Username
	return a, true
}

func companionUsername(ctx context.Context, cfg config) string {
	if a, ok := ctx.Value(navidromeIdentityKey{}).(navidromeIdentity); ok {
		return a.User
	}
	return cfg.navidromeUsername // Administrative Bearer calls and background work.
}

// Jam hosts authenticate with their existing Navidrome account, never the companion admin token.
func authenticateJamHost(r *http.Request, cfg config, client *http.Client) bool {
	a, valid := navidromeRequestIdentity(r, cfg)
	if !valid {
		return false
	}
	result, body := fetch(r.Context(), client, http.MethodGet,
		a.endpoint(cfg, "ping"), nil, nil)
	if result.HTTPStatus != http.StatusOK || result.Error != "" {
		return false
	}
	var response struct {
		Subsonic struct {
			Status string `json:"status"`
		} `json:"subsonic-response"`
	}
	return json.Unmarshal(body, &response) == nil && response.Subsonic.Status == "ok"
}
