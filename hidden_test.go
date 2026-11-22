package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHiddenJobsLeaveTheListAndComeBack(t *testing.T) {
	a := &acquisitionService{cfg: config{downloadPath: t.TempDir()}, jobs: map[string]*acquisitionJob{}}
	id := strings.Repeat("b", 32)
	if err := a.update(&acquisitionJob{ID: id, Artist: "A", Title: "T", Kind: "album", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	post := func(body string) {
		w := httptest.NewRecorder()
		a.setHidden(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
	}
	count := func() int {
		w := httptest.NewRecorder()
		a.list(w, httptest.NewRequest(http.MethodGet, "/", nil))
		return strings.Count(w.Body.String(), `"ID"`)
	}
	post(`{"ids":["` + id + `"],"hidden":true}`)
	if count() != 0 {
		t.Fatal("hidden job still listed")
	}
	post(`{"ids":["` + id + `"],"hidden":false}`)
	if count() != 1 {
		t.Fatal("restored job missing")
	}
}
