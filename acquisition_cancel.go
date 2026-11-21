package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (a *acquisitionService) cancelJob(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	job := a.jobs[r.PathValue("jobId")]
	if job == nil {
		a.mu.Unlock()
		writeJSON(w, 404, map[string]string{"error": "job_not_found"})
		return
	}
	if job.Status == "cancelled" || job.CancelRequested {
		snapshot := *job
		a.mu.Unlock()
		writeJSON(w, 200, snapshot)
		return
	}
	if job.Status == "importing" || job.Status == "moved" || job.Status == "rescanned" {
		a.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "import_already_started"})
		return
	}
	// A recovered import may already be visible even if its last status write failed.
	if complete, _ := a.libraryCopyComplete(job); complete {
		a.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "import_already_started"})
		return
	}
	snapshot := *job
	snapshot.CancelRequested, snapshot.Status, snapshot.Error = true, "cancelling", ""
	err := a.updateLocked(&snapshot)
	stop := a.workers[job.ID]
	a.mu.Unlock()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "job_storage_failed"})
		return
	}
	if stop != nil {
		stop()
	} else {
		go a.run(snapshot.ID)
	}
	writeJSON(w, 202, snapshot)
}

// The worker has stopped before cleanup. Persisted intent survives restarts and
// upstream outages; no other source is tried while cancellation is pending.
func (a *acquisitionService) finishCancellation(j *acquisitionJob) {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	err := func() error {
		if j.Source == "slskd" {
			if j.BatchID == "" && len(j.Files) > 0 {
				if err := a.recoverBatch(ctx, j); err != nil {
					return err
				}
			}
			if err := a.nextSlskPeer(ctx, j); err != nil {
				return err
			}
		}
		if err := removeCancelledDirectory(filepath.Join(a.cfg.downloadPath, "glaze", j.ID), j.ID); err != nil {
			return err
		}
		return removeCancelledDirectory(filepath.Join(a.cfg.libraryPath, libraryFolderName(j.Artist), ".glaze-"+j.ID), j.ID)
	}()
	j.Status, j.Error = "cancelled", ""
	if err == nil {
		j.BatchID, j.Username, j.Files = "", "", nil
	}
	if err != nil {
		j.Status, j.Error = "cancelling", "Waiting for the source to stop safely"
	}
	_ = a.update(j)
	if err != nil && a.ctx.Err() == nil {
		time.AfterFunc(30*time.Second, func() {
			if a.ctx.Err() == nil {
				a.run(j.ID)
			}
		})
	}
}

// A validated job ID names a private staging directory. Reject symlinks rather
// than following them, and validate the entire tree before deleting any file.
func removeCancelledDirectory(root, id string) error {
	if !validJobID(id) || filepath.Base(root) != id && filepath.Base(root) != ".glaze-"+id {
		return errors.New("unsafe_cancel_directory")
	}
	if filepath.Base(root) == ".glaze-"+id {
		info, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe_cancel_directory")
		}
		parent, err := os.Lstat(filepath.Dir(root))
		if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe_cancel_directory")
		}
		marker, err := os.ReadFile(filepath.Join(root, ".glaze-job"))
		if err != nil || string(marker) != id {
			return errors.New("unsafe_cancel_directory")
		}
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("unsafe_cancel_file")
		}
		paths = append(paths, path)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Remove(paths[i]); err != nil {
			return err
		}
	}
	return nil
}
