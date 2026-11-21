package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type qobuzResult struct {
	Source    string `json:"source"`
	MediaType string `json:"media_type"`
	ID        string `json:"id"`
	Desc      string `json:"desc"`
}

func bestQobuz(results []qobuzResult, artist, title, kind string) string {
	bestID, bestScore := "", 0.0
	for _, result := range results {
		if result.Source != "qobuz" || result.MediaType != kind || result.ID == "" {
			continue
		}
		parts := strings.SplitN(result.Desc, " by ", 2)
		if len(parts) != 2 || normalize(parts[0]) != normalize(title) || !sameReleaseVariant(parts[0], title) {
			continue
		}
		titleScore, artistScore := similarity(title, parts[0]), similarity(artist, parts[1])
		if titleScore < 0.72 || artistScore < 0.7 {
			continue
		}
		if score := titleScore*0.7 + artistScore*0.3; score > bestScore {
			bestID, bestScore = result.ID, score
		}
	}
	return bestID
}

func (a *acquisitionService) qobuzCommand(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	configureQobuzCancellation(command)
	command.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(a.cfg.downloadPath, "glaze", ".config"))
	output, err := command.CombinedOutput()
	if err != nil {
		// streamrip can include account details in its error output; never log them.
		_ = output
		return fmt.Errorf("qobuz_command_failed: %w", err)
	}
	return nil
}

func (a *acquisitionService) processQobuz(ctx context.Context, j *acquisitionJob) error {
	if a.cfg.qobuzUsername == "" || a.cfg.qobuzPassword == "" {
		return errors.New("qobuz_not_configured")
	}
	jobDir := filepath.Join(a.cfg.downloadPath, "glaze", j.ID)
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(a.cfg.downloadPath, "glaze", ".config"), 0700); err != nil {
		return err
	}
	configPath := filepath.Join(jobDir, "qobuz.toml")
	defer os.Remove(configPath)
	if err := a.qobuzCommand(ctx, "python3", "/usr/local/lib/glaze/qobuz_config.py", configPath, jobDir); err != nil {
		return err
	}
	baseArgs := []string{"rip", "--config-path", configPath, "--no-db", "--no-progress"}
	if j.Status == "searching" {
		resultsPath := filepath.Join(jobDir, "qobuz-results.json")
		defer os.Remove(resultsPath)
		args := append(append([]string{}, baseArgs...), "search", "--output-file", resultsPath,
			"qobuz", j.Kind, j.Artist+" "+j.Title)
		if err := a.qobuzCommand(ctx, args...); err != nil {
			return err
		}
		data, err := os.ReadFile(resultsPath)
		if err != nil || len(data) > 1<<20 {
			return errors.New("qobuz_search_results_invalid")
		}
		var results []qobuzResult
		if json.Unmarshal(data, &results) != nil {
			return errors.New("qobuz_search_results_invalid")
		}
		j.BatchID = bestQobuz(results, j.Artist, j.Title, j.Kind)
		if j.BatchID == "" {
			return errors.New("qobuz_no_matching_release")
		}
		j.Status = "downloading"
		if err := a.update(j); err != nil {
			return err
		}
	}
	if j.Status == "downloading" {
		args := append(append([]string{}, baseArgs...), "id", "qobuz", j.Kind, j.BatchID)
		if err := a.qobuzCommand(ctx, args...); err != nil {
			return err
		}
		albumDir := filepath.Join(jobDir, "album")
		entries, err := os.ReadDir(albumDir)
		if err != nil {
			return errors.New("qobuz_download_missing")
		}
		expected := 1
		if j.Kind == "album" {
			expected, err = a.jobTrackCount(ctx, j)
			if err != nil {
				return fmt.Errorf("catalogue_tracklist_unavailable: %w", err)
			}
		}
		if len(entries) != expected {
			return errors.New("qobuz_download_file_count_mismatch")
		}
		j.Files = make([]slskFile, 0, len(entries))
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(entry.Name()), ".flac") || info.Size() < 100_000 {
				return errors.New("qobuz_download_file_invalid")
			}
			j.Files = append(j.Files, slskFile{Filename: entry.Name(), Size: info.Size()})
		}
		if err := a.update(j); err != nil {
			return err
		}
		if err := a.moveFrom(ctx, j, albumDir); err != nil {
			return err
		}
		j.Status = "moved"
		if err := a.update(j); err != nil {
			return err
		}
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
