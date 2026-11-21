package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Acquisition jobs are intentionally confined to one active slskd batch and one
// companion-owned download directory. No existing slskd download is touched.
type acquisitionJob struct {
	ID, Artist, Title, Kind, Status, Error string
	Source, BatchID, Username              string
	Files                                  []slskFile
	CatalogueAlbumID                       string
	AlbumTitle                             string
	ExpectedTracks                         int
	CreatedAt, UpdatedAt                   time.Time
	FailedPeers                            []string
	SwitchPeer                             bool
	LastProgressAt                         time.Time
	BytesTransferred                       int64
	ReviewSummary                          string
	CanApproveMetadata, ApproveMetadata    bool
	ReviewFiles, ApprovedFiles             map[string]string
	CancelRequested                        bool
	// Forced skips the library check: the user said to download it anyway.
	Forced bool
}

type slskFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	BitRate  int    `json:"bitRate"`
	Length   int    `json:"length"`
}

type slskResponse struct {
	Username          string     `json:"username"`
	HasFreeUploadSlot bool       `json:"hasFreeUploadSlot"`
	Files             []slskFile `json:"files"`
}

type slskBatch struct {
	ID        string                       `json:"id"`
	Username  string                       `json:"username"`
	Options   struct{ Destination string } `json:"options"`
	Transfers []struct {
		ID               string `json:"id"`
		Filename         string `json:"filename"`
		State            string `json:"state"`
		BytesTransferred int64  `json:"bytesTransferred"`
	} `json:"transfers"`
}

type acquisitionService struct {
	cfg               config
	ctx               context.Context
	client            *http.Client
	mu                sync.Mutex
	submitMu          sync.Mutex
	scanMu            sync.Mutex
	jobs              map[string]*acquisitionJob
	readMetadata      func(context.Context, string) (audioMetadata, error)
	mbMu              sync.Mutex
	nextMB            time.Time
	catalogueMu       sync.Mutex
	catalogue         map[string]cachedCatalogueAlbum
	fingerprintMu     sync.Mutex
	nextFingerprint   time.Time
	repairMetadata    func(context.Context, string, repairTrack, string) error
	lookupFingerprint func(context.Context, string) ([]fingerprintResult, error)
	workers           map[string]context.CancelFunc
	// slots bounds how many jobs search and download at once; nil in tests means unbounded.
	slots chan struct{}
}

func registerAcquisition(mux *http.ServeMux, ctx context.Context, cfg config) *acquisitionService {
	a := &acquisitionService{cfg: cfg, ctx: ctx, jobs: make(map[string]*acquisitionJob),
		client: &http.Client{Timeout: 25 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	a.slots = make(chan struct{}, 3)
	a.readMetadata = readAudioMetadata
	a.repairMetadata = repairDownloadedMetadata
	a.restore()
	go a.sweepFailed()
	mux.HandleFunc("POST /api/v1/acquisition/jobs", a.create)
	mux.HandleFunc("GET /api/v1/acquisition/jobs", a.list)
	mux.HandleFunc("GET /api/v1/acquisition/jobs/{jobId}", a.get)
	mux.HandleFunc("POST /api/v1/acquisition/jobs/{jobId}/cancel", a.cancelJob)
	mux.HandleFunc("POST /api/v1/acquisition/jobs/{jobId}/decision", a.decide)
	mux.HandleFunc("GET /api/v1/acquisition/jobs/preview/{albumId}", a.previewAlbum)
	return a
}

func (a *acquisitionService) providers() []string {
	providers := make([]string, 0, 2)
	for _, provider := range a.cfg.providerOrder {
		if provider == "slskd" && a.cfg.slskdURL == "" {
			continue
		}
		if provider != "qobuz" || (a.cfg.qobuzUsername != "" && a.cfg.qobuzPassword != "") {
			providers = append(providers, provider)
		}
	}
	return providers
}

func (a *acquisitionService) create(w http.ResponseWriter, r *http.Request) {
	if a.cfg.downloadPath == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "downloads_disabled"})
		return
	}
	var input struct {
		Artist, Title, Kind string
		Album               string
		CatalogueAlbumID    string `json:"catalogue_album_id"`
		RetryID             string `json:"retry_id"`
		ApproveMetadata     bool   `json:"approve_metadata"`
		DurationSec         int    `json:"duration_sec"`
		Force               bool   `json:"force"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.ApproveMetadata && input.RetryID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	input.Artist, input.Title, input.Album = strings.TrimSpace(input.Artist), strings.TrimSpace(input.Title), strings.TrimSpace(input.Album)
	retrying := false
	if input.RetryID != "" {
		a.submitMu.Lock()
		a.mu.Lock()
		previous, ok := a.jobs[input.RetryID]
		var saved acquisitionJob
		if ok {
			saved = *previous
		}
		a.mu.Unlock()
		if !ok {
			a.submitMu.Unlock()
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "job_not_found"})
			return
		}
		if (saved.Status != "failed" && saved.Status != "needs_review" && saved.Status != "cancelled") || saved.CancelRequested && saved.Status != "cancelled" || saved.Error == "slskd_enqueue_failed" || (input.ApproveMetadata && (saved.Status != "needs_review" || !saved.CanApproveMetadata)) {
			a.submitMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "download_already_active_or_pending_reconciliation"})
			return
		}
		if input.ApproveMetadata {
			saved.ApproveMetadata = true
			saved.ApprovedFiles = maps.Clone(saved.ReviewFiles)
		}
		// Resume a known batch after a connection/import failure instead of
		// submitting another transfer. A terminal failed slskd batch needs a new job.
		if saved.BatchID != "" && saved.Error != "slskd_download_failed" {
			saved.Status, saved.Error = "downloading", ""
			err := a.update(&saved)
			a.submitMu.Unlock()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "job_storage_failed"})
				return
			}
			go a.run(saved.ID)
			writeJSON(w, http.StatusAccepted, saved)
			return
		}
		a.submitMu.Unlock()
		// A person pressed "try again": first see whether the library has it after all
		// (the usual reason an old failure is still listed), then retry past the cooldown.
		if !input.ApproveMetadata {
			if covered := a.coveredByLibrary(r.Context(), saved); covered != nil {
				writeJSON(w, http.StatusAccepted, covered)
				return
			}
			retrying = true
		}
		input.Artist, input.Title, input.Kind, input.CatalogueAlbumID, input.Album = saved.Artist, saved.Title, saved.Kind, saved.CatalogueAlbumID, saved.AlbumTitle
	}
	if input.Kind == "" {
		input.Kind = "track"
	}
	if !safeName(input.Artist) || !safeName(input.Title) || (input.Kind != "track" && input.Kind != "album") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if !input.Force && input.RetryID == "" {
		if recorded := a.recordLibraryDecision(r.Context(), input.Artist, input.Title, input.Kind, input.Album, input.DurationSec); recorded != nil {
			writeJSON(w, http.StatusAccepted, recorded)
			return
		}
	}
	job, err := a.enqueueCatalogueOpt(input.Artist, input.Title, input.Kind, input.CatalogueAlbumID, input.Album, retrying)
	if err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "queue_full" {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// Manual adds and due pre-saves use the same deduplication, storage and provider chain.
func (a *acquisitionService) enqueue(artist, title, kind string) (*acquisitionJob, error) {
	return a.enqueueCatalogue(artist, title, kind, "", "")
}

func (a *acquisitionService) enqueueCatalogue(artist, title, kind, catalogueID, albumTitle string) (*acquisitionJob, error) {
	return a.enqueueCatalogueOpt(artist, title, kind, catalogueID, albumTitle, false)
}

// enqueueCatalogueOpt: [retry] means a person asked again, so a recent failure doesn't suppress the new job.
func (a *acquisitionService) enqueueCatalogueOpt(artist, title, kind, catalogueID, albumTitle string, retry bool) (*acquisitionJob, error) {
	if (albumTitle != "" && !safeName(albumTitle)) || a.cfg.downloadPath == "" || !safeName(artist) || !safeName(title) || (kind != "album" && kind != "track") {
		return nil, errors.New("invalid_request")
	}
	a.submitMu.Lock()
	defer a.submitMu.Unlock()
	a.mu.Lock()
	for _, existing := range a.jobs {
		if strings.EqualFold(existing.Artist, artist) && strings.EqualFold(existing.Title, title) && existing.Kind == kind && normalize(existing.AlbumTitle) == normalize(albumTitle) && existing.Status != "cancelled" && !existing.CancelRequested && repeatSuppressed(existing, time.Now()) && !(retry && existing.Status == "failed") {
			snapshot := *existing
			a.mu.Unlock()
			return &snapshot, nil
		}
	}
	active := 0
	for _, existing := range a.jobs {
		if existing.Status == "searching" || existing.Status == "downloading" || existing.Status == "importing" {
			active++
		}
	}
	a.mu.Unlock()
	if active >= maxActiveJobs {
		return nil, errors.New("queue_full")
	}
	expected := 0
	if catalogueID != "" {
		if kind != "album" {
			return nil, errors.New("invalid_catalogue_album")
		}
		album, err := a.catalogueAlbum(a.ctx, catalogueID)
		if err != nil {
			return nil, err
		}
		if normalize(album.Title) != normalize(title) || normalize(album.Artist.Name) != normalize(artist) {
			return nil, errors.New("catalogue_album_mismatch")
		}
		expected = album.Count
	}
	providers := a.providers()
	if len(providers) == 0 {
		return nil, errors.New("acquisition_not_configured")
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, errors.New("id_generation_failed")
	}
	now := time.Now().UTC()
	j := &acquisitionJob{ID: hex.EncodeToString(idBytes), Artist: artist, Title: title, Kind: kind,
		CatalogueAlbumID: catalogueID, AlbumTitle: albumTitle, ExpectedTracks: expected,
		Source: providers[0], Status: "searching", CreatedAt: now, UpdatedAt: now}
	if err := a.update(j); err != nil {
		return nil, errors.New("job_storage_failed")
	}
	snapshot := *j
	go a.run(j.ID)
	return &snapshot, nil
}

const maxActiveJobs = 60

// repeatSuppressed: whether asking again for what [j] already covers should just return [j].
// A failure is retried by hand or after a day, not by every repeat request, and an
// "already in your library" answer is trusted for a week.
func repeatSuppressed(j *acquisitionJob, now time.Time) bool {
	age := now.Sub(j.UpdatedAt)
	switch j.Status {
	case "failed":
		return j.Error == "slskd_enqueue_failed" || age < 24*time.Hour
	case "in_library", "skipped":
		return age < 7*24*time.Hour
	}
	return true
}

func (a *acquisitionService) list(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	jobs := make([]acquisitionJob, 0, len(a.jobs))
	for _, j := range a.jobs {
		jobs = append(jobs, *j)
	}
	a.mu.Unlock()
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].CreatedAt.After(jobs[k].CreatedAt) })
	writeJSON(w, http.StatusOK, jobs)
}

func (a *acquisitionService) get(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	j, ok := a.jobs[r.PathValue("jobId")]
	var snapshot acquisitionJob
	if ok {
		snapshot = *j
	}
	a.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job_not_found"})
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (a *acquisitionService) update(j *acquisitionJob) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.updateLocked(j)
}

// Caller holds mu. A worker's older snapshot cannot undo a stop request.
func (a *acquisitionService) updateLocked(j *acquisitionJob) error {
	if a.jobs == nil {
		a.jobs = make(map[string]*acquisitionJob)
	}
	if current := a.jobs[j.ID]; current != nil && current.CancelRequested {
		j.CancelRequested = true
	}
	if j.CancelRequested && j.Status != "cancelled" {
		j.Status = "cancelling"
	}
	j.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	jobsDir := filepath.Join(a.cfg.downloadPath, "glaze", "jobs")
	if err = os.MkdirAll(jobsDir, 0700); err != nil {
		return err
	}
	tmp := filepath.Join(jobsDir, j.ID+".tmp")
	if err = os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(jobsDir, j.ID+".json")); err != nil {
		return err
	}
	copy := *j
	a.jobs[j.ID] = &copy
	slog.Info("acquisition_status", "job_id", j.ID, "status", j.Status, "source", j.Source)
	return nil
}

func (a *acquisitionService) restore() {
	if a.cfg.downloadPath == "" {
		return
	}
	entries, err := os.ReadDir(filepath.Join(a.cfg.downloadPath, "glaze", "jobs"))
	if err != nil {
		return
	}
	var pending []string
	a.mu.Lock()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.cfg.downloadPath, "glaze", "jobs", entry.Name()))
		var j acquisitionJob
		if err != nil || json.Unmarshal(data, &j) != nil || j.ID+".json" != entry.Name() || !validJobID(j.ID) || !safeName(j.Artist) || !safeName(j.Title) {
			continue
		}
		a.jobs[j.ID] = &j
		if j.Status == "importing" {
			j.Status = "downloading"
		}
		if j.Status != "cancelled" && j.Status != "needs_review" && j.Status != "rescanned" && j.Status != "in_library" && j.Status != "skipped" && (j.Status != "failed" || j.Error == "slskd_enqueue_failed") {
			pending = append(pending, j.ID)
		}
	}
	a.mu.Unlock()
	// Populate the entire map before workers or HTTP handlers can access it.
	for _, id := range pending {
		if a.ctx.Err() != nil {
			return
		}
		go a.run(id)
	}
}

func (a *acquisitionService) run(id string) {
	a.mu.Lock()
	stored := a.jobs[id]
	if stored == nil || stored.Status == "cancelled" || a.workers[id] != nil {
		a.mu.Unlock()
		return
	}
	if a.workers == nil {
		a.workers = make(map[string]context.CancelFunc)
	}
	ctx, stop := context.WithCancel(a.ctx)
	a.workers[id] = stop
	j := *stored
	a.mu.Unlock()
	if a.slots != nil {
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
		case <-ctx.Done():
			stop()
			a.mu.Lock()
			delete(a.workers, id)
			a.mu.Unlock()
			return
		}
	}
	defer func() {
		stop()
		a.mu.Lock()
		delete(a.workers, id)
		pending := a.jobs[id].CancelRequested && a.jobs[id].Status == "cancelling" && !j.CancelRequested
		a.mu.Unlock()
		if pending && a.ctx.Err() == nil {
			time.AfterFunc(time.Second, func() { a.run(id) })
		}
	}()
	if j.CancelRequested {
		a.finishCancellation(&j)
		return
	}
	providers := a.providers()
	// Scanning a completed copy does not require its original provider credentials.
	if j.Status == "moved" {
		providers = []string{j.Source}
	} else if len(j.Files) > 0 {
		if complete, _ := a.libraryCopyComplete(&j); complete {
			providers = []string{j.Source}
		}
	}
	index := slices.Index(providers, j.Source)
	if index < 0 && j.Error == "slskd_enqueue_failed" {
		return
	}
	if index < 0 {
		index = 0
	}
	var err error
	if len(providers) == 0 {
		err = errors.New("acquisition_not_configured")
	}
	for i := index; i < len(providers); i++ {
		if j.Source != providers[i] {
			slog.Info("acquisition_provider_fallback", "job_id", j.ID, "from", j.Source, "to", providers[i])
			j.Source, j.Status, j.Error, j.BatchID, j.Username, j.Files = providers[i], "searching", "", "", "", nil
			if err = a.update(&j); err != nil {
				break
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		err = a.process(attemptCtx, &j)
		cancel()
		a.mu.Lock()
		j.CancelRequested = j.CancelRequested || a.jobs[id].CancelRequested
		a.mu.Unlock()
		if j.CancelRequested {
			a.finishCancellation(&j)
			return
		}
		if err != nil && len(j.Files) > 0 && j.Status != "moved" {
			if complete, _ := a.libraryCopyComplete(&j); complete {
				j.Status = "moved"
				_ = a.update(&j)
			}
		}
		if errors.Is(err, errMetadataReview) || err == nil || j.Status == "moved" || a.ctx.Err() != nil || j.Error == "slskd_enqueue_failed" || j.SwitchPeer {
			break
		}
	}
	if err != nil {
		// A shutdown is resumable; a rescan failure must not download again.
		if a.ctx.Err() != nil {
			return
		}
		if j.Status != "moved" && !j.SwitchPeer {
			j.Status = "failed"
			if errors.Is(err, errMetadataReview) {
				j.Status = "needs_review"
			}
		}
		if j.Error != "slskd_enqueue_failed" {
			j.Error = err.Error()
		}
		_ = a.update(&j)
		if j.Status == "moved" || j.SwitchPeer {
			time.AfterFunc(30*time.Second, func() {
				if a.ctx.Err() == nil {
					a.run(id)
				}
			})
		}
	}
}

func (a *acquisitionService) process(ctx context.Context, j *acquisitionJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.Status != "moved" && len(j.Files) > 0 {
		complete, err := a.libraryCopyComplete(j)
		if err != nil {
			return err
		}
		if complete {
			j.Status = "moved"
			if err := a.update(j); err != nil {
				return err
			}
		}
	}
	if j.Status == "moved" {
		if len(j.Files) > 0 {
			source := filepath.Join(a.cfg.downloadPath, "glaze", j.ID)
			if j.Source == "qobuz" {
				source = filepath.Join(source, "album")
			}
			if err := a.removeMovedSource(j, source); err != nil {
				slog.Warn("acquisition_cleanup_pending", "job_id", j.ID)
			}
		}
		if err := a.scanNavidrome(ctx); err != nil {
			return err
		}
		j.Status, j.Error = "rescanned", ""
		return a.update(j)
	}
	if j.Source == "qobuz" {
		return a.processQobuz(ctx, j)
	}
	for {
		if j.SwitchPeer {
			if err := a.nextSlskPeer(ctx, j); err != nil {
				return err
			}
		}
		if j.Status == "failed" && j.Error == "slskd_enqueue_failed" {
			if err := a.recoverBatch(ctx, j); err != nil {
				return err
			}
		}
		if j.Status == "searching" {
			expected := 0
			if j.Kind == "album" {
				var err error
				expected, err = a.jobTrackCount(ctx, j)
				if err != nil {
					return fmt.Errorf("catalogue_tracklist_unavailable: %w", err)
				}
			}
			token, err := a.slskToken(ctx)
			if err != nil {
				return err
			}
			username, files, searchID, err := a.searchSlskd(ctx, token, j.Artist, j.Title, j.Kind, expected, j.AlbumTitle, j.FailedPeers...)
			if err != nil {
				return err
			}
			j.Username, j.Files = username, files
			// Persist the selection before sending a request whose response can be lost.
			j.Status, j.Error = "failed", "slskd_enqueue_failed"
			if err := a.update(j); err != nil {
				return err
			}
			batchRequest := map[string]any{"searchId": searchID, "username": username, "files": files,
				"options": map[string]string{"destination": "glaze/" + j.ID}}
			var response struct {
				Batch    slskBatch `json:"batch"`
				Failures []any     `json:"failures"`
			}
			status, err := a.slskJSON(ctx, token, http.MethodPost, "/api/v0/transfers/downloads/batches", batchRequest, &response)
			// A confirmed offline peer accepted no files. A known partial/failed batch
			// must be retired first; a lost response still goes through reconciliation.
			if status == http.StatusNotFound {
				j.SwitchPeer, j.Error = true, "slskd_download_failed"
				if err := a.update(j); err != nil {
					return err
				}
				continue
			}
			if err == nil && response.Batch.ID != "" && len(response.Failures) > 0 {
				j.BatchID, j.Status, j.Error, j.SwitchPeer = response.Batch.ID, "downloading", "slskd_download_failed", true
				if err := a.update(j); err != nil {
					return err
				}
				continue
			}
			if err != nil || status != http.StatusCreated || len(response.Failures) != 0 || response.Batch.ID == "" {
				j.Status, j.Error = "failed", "slskd_enqueue_failed"
				if err := a.recoverBatch(ctx, j); err != nil {
					return errors.New("slskd_enqueue_failed")
				}
			} else {
				j.BatchID, j.Status, j.Error = response.Batch.ID, "downloading", ""
				if err := a.update(j); err != nil {
					return err
				}
			}
		}
		if j.Status == "downloading" {
			if err := a.waitBatch(ctx, j); err != nil {
				if errors.Is(ctx.Err(), context.Canceled) {
					return err // Shutdown resumes the existing batch.
				}
				// Even on timeout/API failure, retire this batch before trying another
				// peer or provider. A failed cancellation remains resumable.
				j.SwitchPeer, j.Error = true, err.Error()
				if err := a.update(j); err != nil {
					return err
				}
				continue
			}
			if err := a.moveDownload(ctx, j); err != nil {
				return err
			}
			j.Status = "moved"
			if err := a.update(j); err != nil {
				return err
			}
		}
		break
	}
	if j.Status == "moved" {
		if err := a.scanNavidrome(ctx); err != nil {
			return err
		}
		j.Status, j.Error = "rescanned", ""
		return a.update(j)
	}
	return nil
}

func safeName(s string) bool {
	if len(strings.TrimSpace(s)) == 0 || len(s) > 180 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validJobID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}

// Escape path punctuation without changing the original artist/title metadata.
func libraryFolderName(name string) string {
	var out strings.Builder
	for index, r := range name {
		if strings.ContainsRune("%<>:\"/\\|?*", r) || (r == '.' || r == ' ') && (index == 0 || index == len(name)-1) {
			fmt.Fprintf(&out, "%%%02X", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (a *acquisitionService) request(ctx context.Context, method, endpoint string, headers http.Header, body any, output any) (int, error) {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return 0, err
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("upstream_http_%d", res.StatusCode)
	}
	if output != nil {
		return res.StatusCode, json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(output)
	}
	return res.StatusCode, nil
}

func (a *acquisitionService) slskToken(ctx context.Context) (string, error) {
	if a.cfg.slskdAPIKey != "" {
		return a.cfg.slskdAPIKey, nil
	}
	if a.cfg.slskdUsername == "" || a.cfg.slskdPassword == "" {
		return "", errors.New("slskd_credentials_not_configured")
	}
	var response struct {
		Token string `json:"token"`
	}
	_, err := a.request(ctx, http.MethodPost, a.cfg.slskdURL+"/api/v0/session", nil,
		map[string]string{"username": a.cfg.slskdUsername, "password": a.cfg.slskdPassword}, &response)
	if err != nil || response.Token == "" {
		return "", errors.New("slskd_authentication_failed")
	}
	return response.Token, nil
}

func (a *acquisitionService) slskJSON(ctx context.Context, token, method, route string, input, output any) (int, error) {
	header := http.Header{}
	if a.cfg.slskdAPIKey != "" {
		header.Set("X-API-Key", token)
	} else {
		header.Set("Authorization", "Bearer "+token)
	}
	return a.request(ctx, method, a.cfg.slskdURL+route, header, input, output)
}

func (a *acquisitionService) albumTrackCount(ctx context.Context, artist, album string) (int, error) {
	query := url.Values{"query": {fmt.Sprintf("release:\"%s\" AND artist:\"%s\"", album, artist)}, "fmt": {"json"}, "limit": {"5"}}
	var search struct {
		Releases []struct {
			Title        string
			TrackCount   int `json:"track-count"`
			ArtistCredit []struct {
				Name string `json:"name"`
			} `json:"artist-credit"`
		} `json:"releases"`
	}
	err := a.musicBrainz(ctx, "release/?"+query.Encode(), &search)
	if err != nil {
		return 0, err
	}
	for _, release := range search.Releases {
		if normalize(release.Title) != normalize(album) || len(release.ArtistCredit) == 0 || normalize(release.ArtistCredit[0].Name) != normalize(artist) {
			continue
		}
		if release.TrackCount > 0 {
			return release.TrackCount, nil
		}
	}
	return 0, errors.New("release_not_found")
}

func (a *acquisitionService) musicBrainz(ctx context.Context, route string, output any) error {
	a.mbMu.Lock()
	if delay := time.Until(a.nextMB); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			a.mbMu.Unlock()
			return ctx.Err()
		case <-timer.C:
		}
	}
	a.nextMB = time.Now().Add(1100 * time.Millisecond)
	a.mbMu.Unlock()

	_, err := a.request(ctx, http.MethodGet, "https://musicbrainz.org/ws/2/"+route,
		http.Header{"User-Agent": {"BitChordPlusCompanion/" + serviceVersion + " (https://github.com/ILostXD/BitChord_Plus_Companion)"}}, nil, output)
	return err
}

// searchSlskd looks for lossless first: when the plain search only turns up lossy
// files, it searches again for "... flac" before settling for them.
func (a *acquisitionService) searchSlskd(ctx context.Context, token, artist, title, kind string, expected int, albumTitle string, excluded ...string) (string, []slskFile, string, error) {
	user, files, id, err := a.searchSlskdQuery(ctx, token, artist+" "+title, artist, title, kind, expected, albumTitle, excluded...)
	if err == nil && allLossless(files) {
		return user, files, id, nil
	}
	if u2, f2, id2, err2 := a.searchSlskdQuery(ctx, token, artist+" "+title+" flac", artist, title, kind, expected, albumTitle, excluded...); err2 == nil && allLossless(f2) {
		return u2, f2, id2, nil
	}
	return user, files, id, err
}

func (a *acquisitionService) searchSlskdQuery(ctx context.Context, token, query, artist, title, kind string, expected int, albumTitle string, excluded ...string) (string, []slskFile, string, error) {
	var recent []struct {
		ID, SearchText string
		StartedAt      time.Time `json:"startedAt"`
		IsComplete     bool      `json:"isComplete"`
		ResponseCount  int       `json:"responseCount"`
	}
	_, _ = a.slskJSON(ctx, token, http.MethodGet, "/api/v0/searches", nil, &recent)
	sort.Slice(recent, func(i, k int) bool { return recent[i].StartedAt.After(recent[k].StartedAt) })
	for _, found := range recent {
		if found.SearchText == query && found.IsComplete && found.ResponseCount > 0 && time.Since(found.StartedAt) < 15*time.Minute {
			if user, files, err := a.rankSearch(ctx, token, found.ID, artist, title, kind, expected, albumTitle, excluded...); err == nil {
				return user, files, found.ID, nil
			}
		}
	}
	var started struct {
		ID string `json:"id"`
	}
	_, err := a.slskJSON(ctx, token, http.MethodPost, "/api/v0/searches",
		map[string]any{"searchText": query, "searchTimeout": 15_000}, &started)
	if err != nil || started.ID == "" {
		return "", nil, "", errors.New("slskd_search_failed")
	}
	completed := false
	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return "", nil, "", ctx.Err()
		case <-time.After(1 * time.Second):
		}
		var state struct {
			IsComplete bool `json:"isComplete"`
		}
		_, err = a.slskJSON(ctx, token, http.MethodGet, "/api/v0/searches/"+started.ID, nil, &state)
		if err != nil {
			return "", nil, "", err
		}
		if state.IsComplete {
			completed = true
			break
		}
	}
	if !completed {
		return "", nil, "", errors.New("slskd_search_timeout")
	}
	user, files, err := a.rankSearch(ctx, token, started.ID, artist, title, kind, expected, albumTitle, excluded...)
	if err != nil {
		return "", nil, "", err
	}
	return user, files, started.ID, nil
}

func (a *acquisitionService) rankSearch(ctx context.Context, token, id, artist, title, kind string, expected int, albumTitle string, excluded ...string) (string, []slskFile, error) {
	var responses []slskResponse
	_, err := a.slskJSON(ctx, token, http.MethodGet, "/api/v0/searches/"+id+"/responses", nil, &responses)
	if err != nil {
		return "", nil, err
	}
	responses = slices.DeleteFunc(responses, func(r slskResponse) bool { return slices.Contains(excluded, r.Username) })
	user, files, score := rankSlskd(responses, artist, title, kind, expected, a.cfg.formatOrder, albumTitle)
	if score < 0 {
		return "", nil, errors.New("no_suitable_slskd_match")
	}
	return user, files, nil
}

func normalize(s string) string {
	s = strings.ToLower(accentFold.Replace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func similarity(a, b string) float64 {
	a, b = normalize(a), normalize(b)
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	if strings.Contains(" "+a+" ", " "+b+" ") || strings.Contains(" "+b+" ", " "+a+" ") {
		return .9
	}
	wanted := strings.Fields(b)
	found := 0
	for _, word := range wanted {
		if strings.Contains(" "+a+" ", " "+word+" ") {
			found++
		}
	}
	return float64(found) / float64(len(wanted))
}

func audioFormat(f slskFile) int {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(f.Filename, "\\", "/")))
	switch ext {
	case ".flac":
		return 3
	case ".mp3":
		if f.BitRate >= 256 {
			return 2
		}
		return 1
	case ".m4a", ".ogg", ".opus", ".wav", ".aiff":
		return 1
	}
	return 0
}

func reasonable(f slskFile) bool {
	format := audioFormat(f)
	if format == 0 || f.Size < 100000 || f.Size > 1<<30 {
		return false
	}
	if f.Length > 0 {
		kbps := f.Size * 8 / int64(f.Length) / 1000
		if kbps < 64 || kbps > 6000 || (format == 3 && kbps < 350) {
			return false
		}
	}
	return true
}

// losslessTier is larger than every other part of a score put together.
const losslessTier = 10000

func isLossless(f slskFile) bool {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(f.Filename, "\\", "/"))) {
	case ".flac", ".wav", ".aiff", ".aif", ".alac":
		return true
	}
	return false
}

func allLossless(files []slskFile) bool {
	for _, f := range files {
		if !isLossless(f) {
			return false
		}
	}
	return len(files) > 0
}

func rankSlskd(responses []slskResponse, artist, title, kind string, expected int, order []string, albumTitle string) (string, []slskFile, int) {
	bestScore, bestUser, bestFiles := -1, "", []slskFile(nil)
	for _, response := range responses {
		groups := map[string][]slskFile{}
		for _, file := range response.Files {
			if !reasonable(file) {
				continue
			}
			remote := strings.ReplaceAll(file.Filename, "\\", "/")
			key := remote
			if kind == "album" {
				key = path.Dir(remote)
			}
			groups[key] = append(groups[key], file)
		}
		for key, files := range groups {
			if kind == "album" && len(files) != expected {
				continue
			}
			name := key
			if kind == "track" {
				name = strings.TrimSuffix(key, path.Ext(key))
			}
			directory := key
			if kind == "track" {
				directory = path.Dir(key)
			}
			if kind == "track" && albumTitle != "" && (!containsName(directory, albumTitle) || !sameReleaseVariant(strings.ReplaceAll(normalize(directory), normalize(artist), ""), albumTitle)) {
				continue
			}
			if compilationSource(directory) && !compilationSource(artist+"/"+title) {
				continue
			}
			candidateTitle := name
			if kind == "track" {
				candidateTitle = path.Base(name)
			}
			if !containsName(candidateTitle, title) || !sameReleaseVariant(strings.ReplaceAll(normalize(candidateTitle), normalize(artist), ""), title) {
				continue
			}
			if similarity(name, artist) < .45 || similarity(name, title) < .65 {
				continue
			}
			score := int(similarity(name, title)*40) + int(similarity(name, artist)*25)
			// Prefer artist-owned folders and a single's own release over loose copies.
			if kind == "track" && containsName(directory, artist) {
				score += 80
			}
			if kind == "track" && containsName(path.Base(directory), title) {
				score += 80
			}
			format := 3
			for _, file := range files {
				if audioFormat(file) < format {
					format = audioFormat(file)
				}
			}
			class := "other"
			if format == 3 {
				class = "flac"
			} else if format == 2 {
				class = "mp3"
			}
			for i, preference := range order {
				if strings.TrimSpace(preference) == class {
					score += (3 - i) * 20
					break
				}
			}
			if response.HasFreeUploadSlot {
				score += 10
			}
			// Lossless always outranks lossy: a lossy copy wins only when no lossless one matched.
			if allLossless(files) {
				score += losslessTier
			}
			if score > bestScore {
				bestScore, bestUser, bestFiles = score, response.Username, files
			}
		}
	}
	return bestUser, bestFiles, bestScore
}

func (a *acquisitionService) waitBatch(ctx context.Context, j *acquisitionJob) error {
	if j.BatchID == "" {
		return errors.New("missing_slskd_batch")
	}
	for {
		token, err := a.slskToken(ctx)
		if err != nil {
			return err
		}
		var batch slskBatch
		_, err = a.slskJSON(ctx, token, http.MethodGet, "/api/v0/transfers/downloads/batches/"+url.PathEscape(j.BatchID), nil, &batch)
		if err != nil {
			return err
		}
		if len(batch.Transfers) != len(j.Files) {
			return errors.New("slskd_batch_file_count_changed")
		}
		complete := true
		for _, transfer := range batch.Transfers {
			if transfer.State != "Completed, Succeeded" {
				complete = false
				if strings.HasPrefix(transfer.State, "Completed") {
					return errors.New("slskd_download_failed")
				}
			}
		}
		if complete {
			return nil
		}
		if err := a.checkSlskProgress(j, batch, time.Now().UTC()); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Recover a batch whose enqueue response was lost after slskd accepted it.
// Never enqueue a second batch when the first outcome is uncertain.
func (a *acquisitionService) recoverBatch(ctx context.Context, j *acquisitionJob) error {
	token, err := a.slskToken(ctx)
	if err != nil {
		return err
	}
	var user struct {
		Directories []struct {
			Files []struct {
				Filename, BatchID string
			} `json:"files"`
		} `json:"directories"`
	}
	_, err = a.slskJSON(ctx, token, http.MethodGet, "/api/v0/transfers/downloads/"+url.PathEscape(j.Username), nil, &user)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(j.Files))
	for _, file := range j.Files {
		wanted[file.Filename] = true
	}
	counts := map[string]int{}
	for _, directory := range user.Directories {
		for _, file := range directory.Files {
			if wanted[file.Filename] && file.BatchID != "" {
				counts[file.BatchID]++
			}
		}
	}
	for id, count := range counts {
		if count != len(j.Files) {
			continue
		}
		var batch struct {
			ID        string
			Username  string
			Options   struct{ Destination string }
			Transfers []any
		}
		_, err = a.slskJSON(ctx, token, http.MethodGet, "/api/v0/transfers/downloads/batches/"+url.PathEscape(id), nil, &batch)
		if err == nil && batch.Username == j.Username && batch.Options.Destination == "glaze/"+j.ID && len(batch.Transfers) == len(j.Files) {
			j.BatchID, j.Status, j.Error = id, "downloading", ""
			return a.update(j)
		}
	}
	return errors.New("slskd_batch_not_recoverable")
}

func (a *acquisitionService) moveDownload(ctx context.Context, j *acquisitionJob) error {
	source := filepath.Join(a.cfg.downloadPath, "glaze", j.ID)
	return a.moveFrom(ctx, j, source)
}

func (a *acquisitionService) moveFrom(ctx context.Context, j *acquisitionJob, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validJobID(j.ID) || !safeName(j.Artist) || !safeName(j.Title) || len(j.Files) == 0 {
		return errors.New("invalid_job")
	}
	complete, err := a.libraryCopyComplete(j)
	if err != nil {
		return err
	}
	if complete {
		return a.removeMovedSource(j, source)
	}
	if info, err := os.Lstat(source); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("download_directory_missing_or_unsafe")
	}
	entries, err := os.ReadDir(source)
	if err != nil || len(entries) != len(j.Files) {
		return errors.New("download_file_count_mismatch")
	}
	wanted, err := expectedDownloadFiles(j)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		size, known := wanted[entry.Name()]
		if err != nil || entry.Type() != 0 || !info.Mode().IsRegular() || !known || info.Size() != size {
			return errors.New("download_file_mismatch")
		}
	}
	var repairs map[string]repairTrack
	if a.readMetadata != nil {
		metadata := make(map[string]audioMetadata)
		invalid := false
		for _, entry := range entries {
			data, err := a.readMetadata(ctx, filepath.Join(source, entry.Name()))
			if err != nil {
				jobReviewUnreadable(j)
				return errMetadataReview
			}
			metadata[entry.Name()] = data
			invalid = invalid || validateDownloadMetadata(j, data) != nil
		}
		if invalid {
			repairs, err = a.recoverMetadata(ctx, j, source, entries, metadata)
			if err != nil {
				return err
			}
		}
	}

	// The import boundary is atomic with cancelJob: after this point we finish
	// the library copy instead of risking a half-import or deleting library data.
	a.mu.Lock()
	if current := a.jobs[j.ID]; current != nil && current.CancelRequested {
		a.mu.Unlock()
		return context.Canceled
	}
	j.Status = "importing"
	err = a.updateLocked(j)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	artistDir := filepath.Join(a.cfg.libraryPath, libraryFolderName(j.Artist))
	if err := os.MkdirAll(artistDir, 0755); err != nil {
		return err
	}
	artistInfo, err := os.Lstat(artistDir)
	if err != nil || !artistInfo.IsDir() || artistInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe_artist_directory")
	}
	destination := filepath.Join(artistDir, libraryFolderName(j.Title))
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("library_destination_exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging := filepath.Join(artistDir, ".glaze-"+j.ID)
	if err := os.Mkdir(staging, 0700); errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(staging)
		marker, markerErr := os.ReadFile(filepath.Join(staging, ".glaze-job"))
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || markerErr != nil || string(marker) != j.ID {
			return errors.New("unsafe_staging_directory")
		}
		// Remove only files belonging to this job's interrupted copy; never recurse.
		previous, readErr := os.ReadDir(staging)
		if readErr != nil {
			return readErr
		}
		wanted := map[string]bool{".glaze-job": true, ".glaze-tags.json": true}
		for _, entry := range entries {
			wanted[entry.Name()] = true
		}
		for _, entry := range previous {
			if (!wanted[entry.Name()] && !strings.HasPrefix(entry.Name(), ".tagging-pending-")) || entry.Type() != 0 {
				return errors.New("unsafe_staging_file")
			}
		}
		for _, entry := range previous {
			if err := os.Remove(filepath.Join(staging, entry.Name())); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, ".glaze-job"), []byte(j.ID), 0644); err != nil {
		return err
	}
	// A failed copy leaves staging for inspection, not a partially visible album.
	for _, entry := range entries {
		in, err := os.Open(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(staging, entry.Name()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			_ = in.Close()
			return err
		}
		written, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		_ = in.Close()
		if copyErr != nil || closeErr != nil || written != entrySize(entry) {
			return errors.New("library_copy_failed")
		}
	}

	if repairs != nil {
		receipt := taggingReceipt{Version: 1, JobID: j.ID, Files: make(map[string]taggingReceiptFile)}
		for _, entry := range entries {
			filename := filepath.Join(staging, entry.Name())
			repair := a.repairMetadata
			if repair == nil {
				repair = repairDownloadedMetadata
			}
			if err := repair(ctx, filename, repairs[entry.Name()], j.Artist); err != nil {
				return err
			}
			info, err := os.Stat(filename)
			if err != nil {
				return err
			}
			digest, err := taggingDigestContext(ctx, filename)
			if err != nil {
				return err
			}
			receipt.Files[entry.Name()] = taggingReceiptFile{OriginalSize: wanted[entry.Name()], Size: info.Size(), SHA256: digest}
		}
		data, _ := json.Marshal(receipt)
		if err := os.WriteFile(filepath.Join(staging, ".glaze-tags.json"), data, 0644); err != nil {
			return err
		}
	}

	if err := os.Chmod(staging, 0755); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	// The library copy is complete and visible before removing our own staging
	// download; no unrelated files or slskd records are deleted.
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(source, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(source)
}

// Only a complete copy carrying this job's marker is safe to reuse after a crash.
func (a *acquisitionService) libraryCopyComplete(j *acquisitionJob) (bool, error) {
	artist := filepath.Join(a.cfg.libraryPath, libraryFolderName(j.Artist))
	destination := filepath.Join(artist, libraryFolderName(j.Title))
	for _, directory := range []string{artist, destination} {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("unsafe_library_directory")
		}
	}
	marker := filepath.Join(destination, ".glaze-job")
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.New("library_destination_exists")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != j.ID {
		return false, errors.New("library_destination_exists")
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return false, err
	}
	wanted, err := expectedDownloadFiles(j)
	if err != nil {
		return false, err
	}
	receipt, err := readTaggingReceipt(destination, j, wanted)
	if err != nil {
		return false, err
	}
	extra := 1
	if receipt != nil {
		extra++
	}
	if len(entries) != len(wanted)+extra {
		return false, errors.New("library_copy_incomplete")
	}
	for _, entry := range entries {
		if entry.Name() == ".glaze-job" || receipt != nil && entry.Name() == ".glaze-tags.json" {
			continue
		}
		info, err := entry.Info()
		size, known := wanted[entry.Name()]
		if tagged, ok := receipt[entry.Name()]; ok {
			size = tagged.Size
		}
		if err != nil || entry.Type() != 0 || !info.Mode().IsRegular() || !known || info.Size() != size {
			return false, errors.New("library_copy_incomplete")
		}
		if tagged, ok := receipt[entry.Name()]; ok {
			if digest, err := taggingDigest(filepath.Join(destination, entry.Name())); err != nil || digest != tagged.SHA256 {
				return false, errors.New("library_copy_incomplete")
			}
		}
	}
	return true, nil
}

func (a *acquisitionService) removeMovedSource(j *acquisitionJob, source string) error {
	complete, err := a.libraryCopyComplete(j)
	if err != nil || !complete {
		return errors.New("library_copy_incomplete")
	}
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe_download_directory")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	wanted, err := expectedDownloadFiles(j)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		size, known := wanted[entry.Name()]
		if err != nil || entry.Type() != 0 || !info.Mode().IsRegular() || !known || info.Size() != size {
			return errors.New("unexpected_download_file")
		}
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(source, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(source)
}

func expectedDownloadFiles(j *acquisitionJob) (map[string]int64, error) {
	wanted := make(map[string]int64, len(j.Files))
	for _, file := range j.Files {
		name := path.Base(strings.ReplaceAll(file.Filename, "\\", "/"))
		if _, duplicate := wanted[name]; duplicate || audioFormat(file) == 0 || file.Size <= 0 {
			return nil, errors.New("invalid_download_selection")
		}
		wanted[name] = file.Size
	}
	if len(wanted) == 0 {
		return nil, errors.New("invalid_download_selection")
	}
	return wanted, nil
}

func entrySize(entry os.DirEntry) int64 {
	info, err := entry.Info()
	if err != nil {
		return -1
	}
	return info.Size()
}

func (a *acquisitionService) navidromeEndpoint(route string) (string, error) {
	if a.cfg.navidromeUsername == "" || a.cfg.navidromePassword == "" {
		return "", errors.New("navidrome_credentials_not_configured")
	}
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", err
	}
	salt := hex.EncodeToString(saltBytes)
	token := md5.Sum([]byte(a.cfg.navidromePassword + salt)) // Subsonic protocol.
	params := url.Values{"u": {a.cfg.navidromeUsername}, "s": {salt}, "t": {hex.EncodeToString(token[:])},
		"v": {"1.16.1"}, "c": {"BitChordPlusCompanion"}, "f": {"json"}}
	return a.cfg.navidromeURL + "/rest/" + route + ".view?" + params.Encode(), nil
}

func (a *acquisitionService) scanNavidrome(ctx context.Context) error {
	// Each import needs a scan that starts after its files arrive, not a scan already in progress.
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, route := range []string{"startScan", "getScanStatus"} {
		endpoint, err := a.navidromeEndpoint(route)
		if err != nil {
			return err
		}
		var response struct {
			Subsonic struct {
				Status     string `json:"status"`
				ScanStatus struct {
					Scanning bool `json:"scanning"`
				} `json:"scanStatus"`
			} `json:"subsonic-response"`
		}
		_, err = a.request(ctx, http.MethodGet, endpoint, nil, nil, &response)
		if err != nil || response.Subsonic.Status != "ok" {
			return errors.New("navidrome_scan_rejected")
		}
		if route == "startScan" && !response.Subsonic.ScanStatus.Scanning {
			return nil
		}
		if route == "getScanStatus" && !response.Subsonic.ScanStatus.Scanning {
			return nil
		}
		for response.Subsonic.ScanStatus.Scanning {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			endpoint, err = a.navidromeEndpoint("getScanStatus")
			if err != nil {
				return err
			}
			_, err = a.request(ctx, http.MethodGet, endpoint, nil, nil, &response)
			if err != nil || response.Subsonic.Status != "ok" {
				return errors.New("navidrome_scan_status_failed")
			}
		}
		return nil
	}
	return nil
}

func jobReviewUnreadable(job *acquisitionJob) {
	job.CanApproveMetadata = false
	job.ReviewSummary = "The downloaded audio could not be read. It was kept for inspection and was not imported."
}
