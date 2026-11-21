package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type deezerAlbum struct {
	ID     json.Number `json:"id"`
	Title  string      `json:"title"`
	Artist struct {
		Name string `json:"name"`
	} `json:"artist"`
	Count  int `json:"nb_tracks"`
	Tracks struct {
		Data []json.RawMessage `json:"data"`
	} `json:"tracks"`
	Raw map[string]json.RawMessage `json:"-"`
}

type cachedCatalogueAlbum struct {
	album   deezerAlbum
	fetched time.Time
}

func validCatalogueID(id string) bool {
	if len(id) == 0 || len(id) > 20 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// The preview and download verification share the same complete Deezer release.
func (a *acquisitionService) catalogueAlbum(ctx context.Context, id string) (deezerAlbum, error) {
	if !validCatalogueID(id) {
		return deezerAlbum{}, errors.New("invalid_catalogue_id")
	}
	// ponytail: serialize cache misses; use per-ID locks if catalogue throughput grows.
	a.catalogueMu.Lock()
	defer a.catalogueMu.Unlock()
	if hit, ok := a.catalogue[id]; ok && time.Since(hit.fetched) < 24*time.Hour {
		return hit.album, nil
	}
	var raw map[string]json.RawMessage
	_, err := a.request(ctx, http.MethodGet, "https://api.deezer.com/album/"+id, nil, nil, &raw)
	if err != nil {
		return deezerAlbum{}, errors.New("catalogue_unavailable")
	}
	data, err := json.Marshal(raw)
	var album deezerAlbum
	if err != nil || json.Unmarshal(data, &album) != nil || raw["error"] != nil || album.ID.String() != id || album.Count < 1 || album.Count > 2000 || !safeName(album.Title) || !safeName(album.Artist.Name) {
		return deezerAlbum{}, errors.New("catalogue_release_not_found")
	}
	seen := make(map[string]bool)
	entries := album.Tracks.Data
	album.Tracks.Data = nil
	for {
		if len(entries) == 0 {
			return deezerAlbum{}, errors.New("incomplete_catalogue_tracklist")
		}
		for _, data := range entries {
			var track struct {
				ID    json.Number `json:"id"`
				Title string      `json:"title"`
			}
			if json.Unmarshal(data, &track) != nil || !validCatalogueID(track.ID.String()) || track.Title == "" || seen[track.ID.String()] {
				return deezerAlbum{}, errors.New("incomplete_catalogue_tracklist")
			}
			seen[track.ID.String()] = true
		}
		album.Tracks.Data = append(album.Tracks.Data, entries...)
		if len(album.Tracks.Data) >= album.Count {
			break
		}
		var page struct {
			Data []json.RawMessage `json:"data"`
		}
		_, err = a.request(ctx, http.MethodGet, "https://api.deezer.com/album/"+id+"/tracks?index="+strconv.Itoa(len(album.Tracks.Data)), nil, nil, &page)
		if err != nil {
			return deezerAlbum{}, errors.New("catalogue_unavailable")
		}
		entries = page.Data
	}
	if len(album.Tracks.Data) != album.Count {
		return deezerAlbum{}, errors.New("incomplete_catalogue_tracklist")
	}
	raw["tracks"], _ = json.Marshal(album.Tracks)
	album.Raw = raw
	if a.catalogue == nil {
		a.catalogue = make(map[string]cachedCatalogueAlbum)
	}
	if len(a.catalogue) >= 128 {
		var oldest string
		for key, entry := range a.catalogue {
			if oldest == "" || entry.fetched.Before(a.catalogue[oldest].fetched) {
				oldest = key
			}
		}
		delete(a.catalogue, oldest)
	}
	a.catalogue[id] = cachedCatalogueAlbum{album, time.Now()}
	return album, nil
}

func (a *acquisitionService) previewAlbum(w http.ResponseWriter, r *http.Request) {
	album, err := a.catalogueAlbum(r.Context(), r.PathValue("albumId"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, album.Raw)
}

func (a *acquisitionService) jobTrackCount(ctx context.Context, job *acquisitionJob) (int, error) {
	if job.ExpectedTracks > 0 {
		return job.ExpectedTracks, nil
	}
	id := job.CatalogueAlbumID
	if id == "" {
		// Older jobs and manual adds have no catalogue ID. Require an exact edition
		// and artist match, never silently substitute a standard album for deluxe.
		var search struct {
			Data []deezerAlbum `json:"data"`
		}
		_, err := a.request(ctx, http.MethodGet, "https://api.deezer.com/search/album?q="+url.QueryEscape(job.Artist+" "+job.Title), nil, nil, &search)
		if err == nil {
			ids := make(map[string]bool)
			var exact string
			for _, album := range search.Data {
				if sameRelease(album, job.Artist, job.Title) && validCatalogueID(album.ID.String()) {
					ids[album.ID.String()] = true
					if normalize(album.Title) == normalize(job.Title) {
						exact = album.ID.String()
					}
				}
			}
			// One edition that matches, or one that matches the written title exactly.
			if len(ids) == 1 {
				for match := range ids {
					id = match
				}
			} else if exact != "" {
				id = exact
			}
		}
	}
	if id != "" {
		album, err := a.catalogueAlbum(ctx, id)
		if err != nil {
			return 0, err
		}
		if !sameRelease(album, job.Artist, job.Title) {
			return 0, errors.New("catalogue_album_mismatch")
		}
		job.CatalogueAlbumID, job.ExpectedTracks = id, album.Count
		if err := a.update(job); err != nil {
			return 0, err
		}
		return album.Count, nil
	}
	return a.albumTrackCount(ctx, job.Artist, job.Title)
}

// sameRelease: the catalogue's album is the one asked for. The title must be the same apart from
// edition labels and accents, and the standard and deluxe editions are never swapped; the artist
// only has to share a credit, so "Loe Shimmy, Don Toliver" finds "Loe Shimmy" and "JAŸ-Z" finds "JAY Z".
func sameRelease(album deezerAlbum, artist, title string) bool {
	return baseTitle(album.Title) == baseTitle(title) && isDeluxe(album.Title) == isDeluxe(title) &&
		overlap(credits(artist), credits(album.Artist.Name))
}
