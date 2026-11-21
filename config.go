package main

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type config struct {
	navidromeURL, navidromeUsername, navidromePassword  string
	slskdURL, slskdAPIKey, slskdUsername, slskdPassword string
	statePath                                           string
	libraryPath                                         string
	downloadPath                                        string
	formatOrder                                         []string
	providerOrder                                       []string
	qobuzUsername, qobuzPassword                        string
	acoustidKey                                         string
	lastfmKey                                           string
	slskdStallTimeout                                   time.Duration
}

func loadConfig() (config, error) {
	c := config{
		navidromeURL:      strings.TrimRight(os.Getenv("NAVIDROME_API_URL"), "/"),
		navidromeUsername: os.Getenv("NAVIDROME_USERNAME"), navidromePassword: os.Getenv("NAVIDROME_PASSWORD"),
		slskdURL:    strings.TrimRight(os.Getenv("SLSKD_API_URL"), "/"),
		slskdAPIKey: os.Getenv("SLSKD_API_KEY"), slskdUsername: os.Getenv("SLSKD_USERNAME"),
		slskdPassword: os.Getenv("SLSKD_PASSWORD"), libraryPath: os.Getenv("NAVIDROME_LIBRARY_PATH"),
		downloadPath:  os.Getenv("SLSKD_DOWNLOAD_PATH"),
		formatOrder:   strings.Split(strings.ToLower(strings.TrimSpace(os.Getenv("ACQUISITION_FORMAT_ORDER"))), ","),
		providerOrder: strings.Split(strings.ToLower(strings.TrimSpace(os.Getenv("ACQUISITION_PROVIDER_ORDER"))), ","),
		qobuzUsername: os.Getenv("QOBUZ_USERNAME"), qobuzPassword: os.Getenv("QOBUZ_PASSWORD"),
		acoustidKey: os.Getenv("ACOUSTID_API_KEY"),
		lastfmKey:   strings.TrimSpace(os.Getenv("LASTFM_API_KEY")),
	}
	// Preserve existing deployments; an explicit false enables the lightweight Jam setup.
	if value := os.Getenv("DOWNLOADS_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return config{}, errors.New("DOWNLOADS_ENABLED must be true or false")
		}
		if !enabled {
			c.libraryPath, c.downloadPath, c.slskdURL = "", "", ""
		}
		if enabled && c.downloadPath == "" {
			return config{}, errors.New("Downloads require SLSKD_DOWNLOAD_PATH")
		}
	}
	c.statePath = os.Getenv("COMPANION_STATE_PATH")
	if c.statePath == "" {
		if c.downloadPath != "" {
			c.statePath = filepath.Join(c.downloadPath, "glaze")
		} else {
			c.statePath = filepath.Join(os.TempDir(), "bitchord-plus-companion")
		}
	}
	if !filepath.IsAbs(c.statePath) || c.statePath == string(filepath.Separator) {
		return config{}, errors.New("COMPANION_STATE_PATH must be a separate absolute path")
	}
	c.slskdStallTimeout = 3 * time.Minute
	if value := os.Getenv("ACQUISITION_STALL_TIMEOUT_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 30 || seconds > 1800 {
			return config{}, errors.New("ACQUISITION_STALL_TIMEOUT_SECONDS must be between 30 and 1800")
		}
		c.slskdStallTimeout = time.Duration(seconds) * time.Second
	}
	for name, value := range map[string]string{"NAVIDROME_API_URL": c.navidromeURL, "SLSKD_API_URL": c.slskdURL} {
		if name == "SLSKD_API_URL" && (c.downloadPath == "" || value == "") {
			continue
		}
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return config{}, errors.New(name + " must be an HTTP(S) base URL without credentials, query, or fragment")
		}
	}
	if c.downloadPath != "" && (!strings.HasPrefix(c.libraryPath, "/") || c.libraryPath == "/") {
		return config{}, errors.New("NAVIDROME_LIBRARY_PATH must be an absolute container path")
	}
	// Acquisition can remain disabled in local/test setups without the writable mount.
	if c.downloadPath != "" && (!strings.HasPrefix(c.downloadPath, "/") || c.downloadPath == "/" || c.downloadPath == c.libraryPath) {
		return config{}, errors.New("SLSKD_DOWNLOAD_PATH must be a separate absolute container path")
	}
	if c.downloadPath != "" && c.slskdURL == "" && (c.qobuzUsername == "" || c.qobuzPassword == "") {
		return config{}, errors.New("Downloads require a configured slskd or Qobuz source")
	}
	if len(c.formatOrder) == 1 && c.formatOrder[0] == "" {
		c.formatOrder = []string{"flac", "mp3", "other"}
	}
	if strings.Join(c.formatOrder, ",") != "flac,mp3,other" && !validFormatOrder(c.formatOrder) {
		return config{}, errors.New("ACQUISITION_FORMAT_ORDER must contain flac,mp3,other exactly once")
	}
	if len(c.providerOrder) == 1 && c.providerOrder[0] == "" {
		c.providerOrder = []string{"slskd", "qobuz"}
	}
	if len(c.providerOrder) != 2 || c.providerOrder[0] == c.providerOrder[1] ||
		(c.providerOrder[0] != "slskd" && c.providerOrder[0] != "qobuz") ||
		(c.providerOrder[1] != "slskd" && c.providerOrder[1] != "qobuz") {
		return config{}, errors.New("ACQUISITION_PROVIDER_ORDER must contain slskd,qobuz exactly once")
	}
	return c, nil
}

func validFormatOrder(order []string) bool {
	if len(order) != 3 {
		return false
	}
	seen := map[string]bool{}
	for _, name := range order {
		name = strings.TrimSpace(name)
		if (name != "flac" && name != "mp3" && name != "other") || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
