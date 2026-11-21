package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Count byte progress across the entire batch: album peers sending any data are
// not abandoned because one remaining file is still in the remote queue.
func (a *acquisitionService) checkSlskProgress(j *acquisitionJob, batch slskBatch, now time.Time) error {
	var bytes int64
	eligible := true
	for _, transfer := range batch.Transfers {
		bytes += transfer.BytesTransferred
		if !strings.HasPrefix(transfer.State, "Completed") &&
			!strings.Contains(transfer.State, "Remotely") && !strings.Contains(transfer.State, "InProgress") {
			eligible = false // Locally queued transfers aren't the remote peer's fault.
		}
	}
	if j.LastProgressAt.IsZero() || bytes != j.BytesTransferred || !eligible {
		j.LastProgressAt, j.BytesTransferred = now, bytes
		return a.update(j)
	}
	timeout := a.cfg.slskdStallTimeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	if now.Sub(j.LastProgressAt) >= timeout {
		return errors.New("slskd_download_stalled")
	}
	return nil
}

// Persisted SwitchPeer is a restart barrier: no new request/provider can run
// until the old owned batch is confirmed terminal and its output is isolated.
func (a *acquisitionService) nextSlskPeer(ctx context.Context, j *acquisitionJob) error {
	if j.BatchID != "" {
		token, err := a.slskToken(ctx)
		if err != nil {
			return err
		}
		stopCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		for {
			var batch slskBatch
			_, err := a.slskJSON(stopCtx, token, http.MethodGet, "/api/v0/transfers/downloads/batches/"+url.PathEscape(j.BatchID), nil, &batch)
			if err != nil {
				return errors.New("slskd_cancel_pending")
			}
			if batch.ID != j.BatchID || batch.Username != j.Username || batch.Options.Destination != "glaze/"+j.ID || len(batch.Transfers) > len(j.Files) {
				return errors.New("slskd_batch_ownership_mismatch")
			}
			stopped := true
			for _, transfer := range batch.Transfers {
				if !slices.ContainsFunc(j.Files, func(file slskFile) bool { return file.Filename == transfer.Filename }) {
					return errors.New("slskd_batch_file_mismatch")
				}
				if strings.HasPrefix(transfer.State, "Completed") {
					continue
				}
				stopped = false
				if transfer.ID == "" {
					return errors.New("slskd_cancel_pending")
				}
				if _, err := a.slskJSON(stopCtx, token, http.MethodDelete,
					"/api/v0/transfers/downloads/"+url.PathEscape(j.Username)+"/"+url.PathEscape(transfer.ID), nil, nil); err != nil {
					return errors.New("slskd_cancel_pending")
				}
			}
			if stopped {
				break
			}
			select {
			case <-stopCtx.Done():
				return errors.New("slskd_cancel_pending")
			case <-time.After(time.Second):
			}
		}
	}
	// Never recurse or delete anything outside this job's known regular files.
	if !validJobID(j.ID) {
		return errors.New("invalid_job")
	}
	source := filepath.Join(a.cfg.downloadPath, "glaze", j.ID)
	info, err := os.Lstat(source)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe_download_directory")
		}
		wanted, err := expectedDownloadFiles(j)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			info, err := entry.Info()
			_, known := wanted[entry.Name()]
			if err != nil || !known || entry.Type() != 0 || !info.Mode().IsRegular() {
				return errors.New("unsafe_download_file")
			}
		}
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(source, entry.Name())); err != nil {
				return err
			}
		}
	}
	j.FailedPeers = append(slices.Clone(j.FailedPeers), j.Username)
	j.Username, j.BatchID, j.Status, j.Error = "", "", "searching", ""
	j.Files, j.SwitchPeer, j.LastProgressAt, j.BytesTransferred = nil, false, time.Time{}, 0
	return a.update(j)
}
