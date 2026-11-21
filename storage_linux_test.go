package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStorageReportsActualDiskAndRequiresAuthentication(t *testing.T) {
	root := t.TempDir()
	handler := newHandler(strings.Repeat("x", 32), context.Background(), config{libraryPath: root, downloadPath: root})
	for _, authorized := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/storage", nil)
		if authorized {
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if !authorized {
			if result.Code != http.StatusUnauthorized {
				t.Fatalf("unprotected storage: %d", result.Code)
			}
			continue
		}
		var response struct {
			Library    storageSpace
			Downloads  storageSpace
			SharedDisk bool
		}
		if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &response) != nil {
			t.Fatalf("storage response: %d %s", result.Code, result.Body.String())
		}
		if !response.SharedDisk || response.Library.TotalBytes == 0 || response.Library.AvailableBytes > response.Library.TotalBytes || response.Library.TotalBytes != response.Downloads.TotalBytes {
			t.Fatalf("incorrect disk stats: %+v", response)
		}
	}
}
