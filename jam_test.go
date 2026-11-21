package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type jamSnapshot struct {
	ID            string         `json:"id"`
	HostID        string         `json:"host_id"`
	Revision      uint64         `json:"revision"`
	Members       []jamMember    `json:"members"`
	Queue         []jamQueueItem `json:"queue"`
	Playback      jamPlayback    `json:"playback"`
	GuestPlayback bool           `json:"guest_playback"`
	ServerTime    int64          `json:"server_time_ms"`
}

type jamIdentity struct {
	Session     jamSnapshot `json:"session"`
	ID          string      `json:"member_id"`
	Token       string      `json:"member_token"`
	InviteToken string      `json:"invite_token"`
}

type jamEvent struct {
	Type       string      `json:"type"`
	Error      string      `json:"error"`
	Nonce      string      `json:"nonce"`
	Reason     string      `json:"reason"`
	Session    jamSnapshot `json:"session"`
	ServerTime int64       `json:"server_time_ms"`
}

func checkJam(t *testing.T, base, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rest := func(method, path, body, memberToken string, want int, result any) {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if path == "/api/v1/jam/sessions" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("X-Jam-Member-Token", memberToken)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s: HTTP %d, want %d", method, response.StatusCode, want)
		}
		if result != nil && json.NewDecoder(response.Body).Decode(result) != nil {
			t.Fatal("invalid REST response")
		}
	}
	var host, guest jamIdentity
	rest("POST", "/api/v1/jam/sessions", `{"name":"Verification host"}`, "", 201, &host)
	path := "/api/v1/jam/sessions/" + host.Session.ID
	// Cleanup only the test's own session, including if a later assertion fails.
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		r, _ := http.NewRequestWithContext(cleanupCtx, "DELETE", base+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Jam-Member-Token", host.Token)
		if response, err := http.DefaultClient.Do(r); err == nil {
			response.Body.Close()
		}
	}()
	if len(host.InviteToken) != 64 {
		t.Fatal("host did not receive a private invite")
	}
	rest("POST", path+"/join", `{"name":"Uninvited","invite_token":"`+strings.Repeat("0", 64)+`"}`, "", 403, nil)
	rest("POST", path+"/join", `{"name":"Verification guest","invite_token":"`+host.InviteToken+`"}`, "", 201, &guest)
	if guest.InviteToken != "" {
		t.Fatal("guest must not receive the reusable invite")
	}
	url := "ws" + strings.TrimPrefix(base, "http") + "/api/v1/jam/ws?session_id=" + host.Session.ID
	for _, tc := range []struct {
		headers http.Header
		status  int
	}{{nil, 403}, {http.Header{"Authorization": {"Bearer " + token}}, 403}} {
		conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: tc.headers})
		if conn != nil {
			conn.CloseNow()
		}
		if response != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != tc.status {
			t.Fatal("WebSocket authentication did not reject invalid credentials")
		}
	}
	read := func(conn *websocket.Conn) jamEvent {
		t.Helper()
		_, data, err := conn.Read(ctx)
		var event jamEvent
		if err != nil || json.Unmarshal(data, &event) != nil {
			t.Fatalf("WebSocket read failed: %v", err)
		}
		return event
	}
	dial := func(memberToken string) (*websocket.Conn, jamEvent) {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
			HTTPHeader: http.Header{"X-Jam-Member-Token": {memberToken}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		conn.SetReadLimit(512 * 1024)
		event := read(conn)
		if event.Type != "connected" || event.Session.ID != host.Session.ID {
			t.Fatal("missing initial session snapshot")
		}
		return conn, event
	}
	hostConn, _ := dial(host.Token)
	guestConn, welcome := dial(guest.Token)
	send := func(conn *websocket.Conn, data string) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat := func(conn *websocket.Conn) {
		t.Helper()
		send(conn, `{"type":"heartbeat","nonce":"round-trip"}`)
		event := read(conn)
		if event.Type != "heartbeat" || event.Nonce != "round-trip" || event.ServerTime <= 0 {
			t.Fatal("heartbeat must echo nonce with server clock")
		}
	}
	checkError := func(conn *websocket.Conn, command, code string) {
		t.Helper()
		send(conn, command)
		event := read(conn)
		if event.Type != "error" || event.Error != code {
			t.Fatalf("want %s error, got %s / %s", code, event.Type, event.Error)
		}
	}
	state := func() jamSnapshot {
		t.Helper()
		a, b := read(hostConn), read(guestConn)
		first, _ := json.Marshal(a.Session)
		second, _ := json.Marshal(b.Session)
		if a.Type != "state" || b.Type != "state" || string(first) != string(second) {
			t.Fatal("both participants must receive the same state/revision")
		}
		return a.Session
	}
	heartbeat(guestConn)
	if !welcome.Session.GuestPlayback {
		t.Fatal("guest playback should default on")
	}
	send(hostConn, `{"type":"guest_controls.set","guest_playback":false}`)
	state()
	checkError(guestConn, `{"type":"playback.next"}`, "host_only")
	checkError(guestConn, `{"type":"queue.add","track_id":"bad id"}`, "invalid_track_id")
	checkError(hostConn, `{"type":"playback.set","track_id":"test","position_ms":-1,"playing":true}`, "invalid_playback")
	checkError(guestConn, `{"type":"unknown"}`, "unknown_command")
	send(hostConn, `{"type":"queue.add","track_id":"verification-first"}`)
	snapshot := state()
	firstID := snapshot.Queue[0].ID
	send(guestConn, `{"type":"queue.add","track_id":"verification-second"}`)
	snapshot = state()
	secondID := snapshot.Queue[1].ID
	checkError(guestConn, `{"type":"queue.remove","item_id":"`+firstID+`"}`, "not_item_owner")
	checkError(guestConn, `{"type":"queue.vote","item_id":"`+secondID+`"}`, "invalid_vote")
	vote := `{"type":"queue.vote","item_id":"` + secondID + `","vote":true}`
	send(guestConn, vote)
	snapshot = state()
	if snapshot.Queue[0].ID != secondID || len(snapshot.Queue[0].Votes) != 1 {
		t.Fatal("vote must promote the queued song")
	}
	send(guestConn, vote)
	heartbeat(guestConn) // An idempotent vote produces no state event.
	var unchanged jamSnapshot
	rest("GET", path, "", guest.Token, 200, &unchanged)
	if unchanged.Revision != snapshot.Revision || len(unchanged.Queue[0].Votes) != 1 {
		t.Fatal("repeat votes must not inflate votes or revisions")
	}
	send(guestConn, `{"type":"queue.vote","item_id":"`+secondID+`","vote":false}`)
	snapshot = state()
	if snapshot.Queue[0].ID != firstID {
		t.Fatal("tied votes must restore insertion order")
	}
	send(guestConn, vote)
	state()
	send(hostConn, `{"type":"playback.set","track_id":"verification-current","position_ms":12000,"playing":true}`)
	snapshot = state()
	if snapshot.Playback.TrackID != "verification-current" || snapshot.Playback.PositionMS != 12000 || !snapshot.Playback.Playing || snapshot.Playback.UpdatedMS <= 0 {
		t.Fatal("host playback and clock anchor did not reach the guest")
	}
	checkError(hostConn, `{"type":"playback.next","expected_revision":0}`, "stale_revision")
	guestConn.CloseNow()
	guestConn, welcome = dial(guest.Token)
	if welcome.Session.Revision != snapshot.Revision || welcome.Session.Playback.TrackID != "verification-current" {
		t.Fatal("reconnect must restore current state")
	}
	send(hostConn, `{"type":"guest_controls.set","guest_playback":true}`)
	snapshot = state()
	for _, action := range []string{"previous", "shuffle", "repeat", "next"} {
		send(guestConn, `{"type":"playback.request","action":"`+action+`","shuffle":true,"repeat":2}`)
		if event := read(hostConn); event.Type != "playback.request" {
			t.Fatal("guest control did not reach the host")
		}
		heartbeat(guestConn) // No playback/queue event until the host confirms it.
		var pending jamSnapshot
		rest("GET", path, "", guest.Token, 200, &pending)
		if pending.Revision != snapshot.Revision || pending.Playback.TrackID != "verification-current" {
			t.Fatal("guest advanced ahead of host")
		}
	}
	send(hostConn, `{"type":"playback.set","track_id":"verification-second","item_id":"`+secondID+`","position_ms":0,"playing":true,"shuffle":true,"repeat":2}`)
	snapshot = state()
	if !snapshot.Playback.Shuffle || snapshot.Playback.Repeat != 2 {
		t.Fatal("shared modes missing")
	}
	if snapshot.Playback.TrackID != "verification-second" || len(snapshot.Queue) != 1 || snapshot.Playback.PositionMS != 0 {
		t.Fatal("next must consume the highest-voted song")
	}
	send(guestConn, `{"type":"queue.add","track_id":"verification-third"}`)
	snapshot = state()
	thirdID := snapshot.Queue[1].ID
	send(guestConn, `{"type":"queue.vote","item_id":"`+firstID+`","vote":true}`)
	state()
	rest("POST", path+"/leave", "", guest.Token, 200, nil)
	snapshot = read(hostConn).Session
	if len(snapshot.Members) != 1 || len(snapshot.Queue[0].Votes) != 0 {
		t.Fatal("leaving must remove the participant and their votes")
	}
	rest("GET", path, "", guest.Token, 403, nil)
	send(hostConn, `{"type":"queue.remove","item_id":"`+thirdID+`"}`)
	if event := read(hostConn); event.Type != "state" || len(event.Session.Queue) != 1 {
		t.Fatal("host must be able to remove a guest's queued song")
	}
	for _, want := range []string{"verification-first", ""} {
		send(hostConn, `{"type":"playback.next"}`)
		if event := read(hostConn); event.Type != "state" || event.Session.Playback.TrackID != want {
			t.Fatal("next must advance, then stop at the end of the queue")
		}
	}
	rest("DELETE", path, "", host.Token, 200, nil)
	if event := read(hostConn); event.Type != "session_ended" {
		t.Fatal("ending session must notify connected participants")
	}
	rest("GET", path, "", host.Token, 404, nil)
	t.Log("Real REST/session + two-client sync, queue, voting, reconnect, leave and cleanup passed")
}

func TestJam(t *testing.T) {
	token := strings.Repeat("x", 32)
	server := httptest.NewServer(newHandler(token, context.Background(), config{}))
	defer server.Close()
	checkJam(t, server.URL, token)
}

func TestJamSocketLimitsAndShutdown(t *testing.T) {
	root, stop := context.WithCancel(context.Background())
	defer stop()
	token := strings.Repeat("x", 32)
	handler := newHandler(token, root, config{})
	server := httptest.NewServer(handler)
	defer server.Close()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/jam/sessions", strings.NewReader(`{"name":"Host"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(w, r)
	var host jamIdentity
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &host) != nil {
		t.Fatal("session creation failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/jam/ws?session_id=" + host.Session.ID
	options := &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": {"Bearer " + token}, "X-Jam-Member-Token": {host.Token},
	}}
	var connections []*websocket.Conn
	for range 4 {
		conn, _, err := websocket.Dial(ctx, url, options)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		connections = append(connections, conn)
		if _, _, err := conn.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	extra, response, err := websocket.Dial(ctx, url, options)
	if extra != nil {
		extra.CloseNow()
	}
	if response != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != 429 {
		t.Fatal("fifth socket must be rejected")
	}
	stop()
	for _, conn := range connections {
		// A final session_ended notification may precede the socket closing.
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				if ctx.Err() != nil {
					t.Fatal("shutdown must promptly close all active sockets")
				}
				break
			}
		}
	}
}

func TestDeployedJam(t *testing.T) {
	base := os.Getenv("COMPANION_TEST_URL")
	if base == "" {
		t.Skip("Set COMPANION_TEST_URL to verify the real deployment")
	}
	checkJam(t, base, os.Getenv("COMPANION_API_TOKEN"))
}

func TestJamSocketExpiryAndInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind websocket.MessageType
		data string
		code websocket.StatusCode
	}{
		{"expired", websocket.MessageText, `{"type":"heartbeat"}`, websocket.StatusNormalClosure},
		{"binary", websocket.MessageBinary, `{}`, websocket.StatusUnsupportedData},
		{"invalid_json", websocket.MessageText, `{`, websocket.StatusInvalidFramePayloadData},
		{"oversized", websocket.MessageText, strings.Repeat("x", 512*1024+1), websocket.StatusMessageTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &jamHub{sessions: make(map[string]*jamSession)}
			mux := http.NewServeMux()
			hub.registerREST(mux)
			root, stop := context.WithCancel(context.Background())
			defer stop()
			mux.HandleFunc("GET /api/v1/jam/ws", hub.socket(root))
			server := httptest.NewServer(mux)
			defer server.Close()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/jam/sessions", strings.NewReader(`{"name":"Host"}`)))
			var host jamIdentity
			_ = json.Unmarshal(w.Body.Bytes(), &host)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/jam/ws?session_id=" + host.Session.ID
			conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"X-Jam-Member-Token": {host.Token}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if _, _, err := conn.Read(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.name == "expired" {
				hub.mu.Lock()
				hub.sessions[host.Session.ID].Expires = time.Now().Add(-time.Second)
				hub.mu.Unlock()
			}
			if err := conn.Write(ctx, tc.kind, []byte(tc.data)); err != nil {
				t.Fatal(err)
			}
			if tc.name == "expired" {
				_, data, err := conn.Read(ctx)
				var event jamEvent
				if err != nil || json.Unmarshal(data, &event) != nil || event.Type != "session_ended" || event.Reason != "expired" {
					t.Fatal("expiry must deliver its terminal event before closing")
				}
			}
			if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != tc.code {
				t.Fatalf("want close code %d, got %d", tc.code, websocket.CloseStatus(err))
			}
		})
	}
}
