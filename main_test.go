package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFoundation(t *testing.T) {
	token := strings.Repeat("x", 32)
	handler := newHandler(token, context.Background(), config{})
	for _, tc := range []struct {
		path   string
		auth   bool
		status int
	}{{"/live", false, 200}, {"/", false, 401}, {"/", true, 200}} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != tc.status {
			t.Fatalf("%s: got %d, want %d", tc.path, result.Code, tc.status)
		}
	}
}

func TestAllRESTStubs(t *testing.T) {
	token := strings.Repeat("x", 32)
	handler := newHandler(token, context.Background(), config{})
	for _, pattern := range stubRoutes {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{sessionId}", "test-session"), "{jobId}", "test-job")
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != http.StatusNotImplemented || !strings.Contains(result.Body.String(), "not_implemented") {
			t.Fatalf("%s: got %d %s", pattern, result.Code, result.Body.String())
		}
	}
}
