package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJamSessions(t *testing.T) {
	token := strings.Repeat("x", 32)
	handler := newHandler(token, context.Background(), config{})
	request := func(method, path, body, memberToken string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Jam-Member-Token", memberToken)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: status %d, want %d", method, path, w.Code, want)
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, body := range []string{`{}`, `{"name":" "}`, `{"name":"host","extra":true}`, `{"name":"host"}{}`, `null`, `{"name":"a\nb"}`} {
		request("POST", "/api/v1/jam/sessions", body, "", 400)
	}
	host := request("POST", "/api/v1/jam/sessions", `{"name":"Host"}`, "", 201)
	session := host["session"].(map[string]any)
	path := "/api/v1/jam/sessions/" + session["id"].(string)
	hostToken := host["member_token"].(string)
	invite := host["invite_token"].(string)
	request("POST", path+"/join", `{"name":"Guest","invite_token":"`+strings.Repeat("0", 64)+`"}`, "", 403)
	guest := request("POST", path+"/join", `{"name":"Guest","invite_token":"`+invite+`"}`, "", 201)
	if _, ok := guest["invite_token"]; ok {
		t.Fatal("guest must not receive invite token")
	}
	guestToken := guest["member_token"].(string)
	snapshot := request("GET", path, "", guestToken, 200)
	encoded, _ := json.Marshal(snapshot)
	if len(snapshot["members"].([]any)) != 2 || strings.Contains(string(encoded), hostToken) || strings.Contains(string(encoded), guestToken) {
		t.Fatal("snapshot must contain both members but no private tokens")
	}
	request("GET", path, "", "", 403)
	other := request("POST", "/api/v1/jam/sessions", `{"name":"Other session"}`, "", 201)
	otherPath := "/api/v1/jam/sessions/" + other["session"].(map[string]any)["id"].(string)
	request("GET", otherPath, "", guestToken, 403)
	request("GET", path, "", other["member_token"].(string), 403)
	request("DELETE", path, "", guestToken, 403)
	request("POST", path+"/leave", "", guestToken, 200)
	request("GET", path, "", guestToken, 403)
	snapshot = request("GET", path, "", hostToken, 200)
	if len(snapshot["members"].([]any)) != 1 {
		t.Fatal("guest must be removed")
	}
	request("POST", path+"/leave", "", hostToken, 200)
	request("GET", path, "", hostToken, 404)
	request("POST", path+"/join", `{"name":"Guest","invite_token":"`+invite+`"}`, "", 404)
}

func TestJamLimitsExpiryAndConcurrency(t *testing.T) {
	hub := &jamHub{sessions: make(map[string]*jamSession)}
	mux := http.NewServeMux()
	hub.registerREST(mux)
	create := func(want int) jamIdentity {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/jam/sessions", strings.NewReader(`{"name":"Host"}`)))
		if w.Code != want {
			t.Fatalf("create: %d, want %d", w.Code, want)
		}
		var identity jamIdentity
		_ = json.Unmarshal(w.Body.Bytes(), &identity)
		return identity
	}
	host := create(201)
	session := hub.sessions[host.Session.ID]
	for range 63 {
		create(201)
	}
	create(429)
	for id, s := range hub.sessions {
		if id != session.ID {
			s.Expires = time.Now().Add(-time.Second)
		}
	}
	create(201)
	if len(hub.sessions) != 2 {
		t.Fatal("expired sessions must release capacity")
	}
	joinBody := `{"name":"Guest","invite_token":"` + host.InviteToken + `"}`
	for i := range 31 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/jam/sessions/"+session.ID+"/join", strings.NewReader(joinBody)))
		if w.Code != 201 {
			t.Fatalf("join %d: %d", i, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/jam/sessions/"+session.ID+"/join", strings.NewReader(joinBody)))
	if w.Code != 429 {
		t.Fatal("member limit must be enforced")
	}
	member := session.Members[host.ID]
	for range 1000 {
		if code := hub.commandLocked(session, member, jamCommand{Type: "queue.add", TrackID: "test"}); code != "" {
			t.Fatal(code)
		}
	}
	if hub.commandLocked(session, member, jamCommand{Type: "queue.add", TrackID: "test"}) != "queue_limit" {
		t.Fatal("queue limit must be enforced")
	}
	itemID := session.Queue[0].ID
	oldSnapshot := session.snapshot()
	var workers sync.WaitGroup
	for _, voter := range session.Members {
		workers.Go(func() {
			hub.mu.Lock()
			defer hub.mu.Unlock()
			vote := true
			if code := hub.commandLocked(session, voter, jamCommand{Type: "queue.vote", ItemID: itemID, Vote: &vote}); code != "" {
				t.Error(code)
			}
		})
	}
	workers.Wait()
	if len(session.Queue[0].Votes) != 32 || len(oldSnapshot["queue"].([]jamQueueItem)[0].Votes) != 0 {
		t.Fatal("concurrent votes must not get lost or mutate past snapshots")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := &jamClient{send: make(chan []byte, 1), cancel: cancel}
	session.clients[slow] = host.ID
	hub.changedLocked(session)
	hub.changedLocked(session)
	if ctx.Err() == nil || len(session.clients) != 0 {
		t.Fatal("slow connections must be cancelled without blocking others")
	}
	ended := &jamClient{send: make(chan []byte, 1), cancel: func() {}}
	session.clients[ended] = host.ID
	session.Expires = time.Now().Add(-time.Second)
	hub.expireLocked()
	var event jamEvent
	_ = json.Unmarshal(<-ended.send, &event)
	if hub.sessions[session.ID] != nil || event.Type != "session_ended" || event.Reason != "expired" {
		t.Fatal("expiration must remove state and notify sockets")
	}
}
