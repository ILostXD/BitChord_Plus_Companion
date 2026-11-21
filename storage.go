package main

import (
	"net/http"
	"os"
)

type storageSpace struct {
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
}

func storageHandler(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		library, libraryErr := diskSpace(cfg.libraryPath)
		downloads, downloadsErr := diskSpace(cfg.downloadPath)
		if libraryErr != nil && downloadsErr != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage_unavailable"})
			return
		}
		libraryInfo, _ := os.Stat(cfg.libraryPath)
		downloadInfo, _ := os.Stat(cfg.downloadPath)
		writeJSON(w, http.StatusOK, struct {
			Library    *storageSpace `json:"library"`
			Downloads  *storageSpace `json:"downloads"`
			SharedDisk bool          `json:"sharedDisk"`
		}{library, downloads, sameDisk(libraryInfo, downloadInfo)})
	}
}
