package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"strconv"
	"testing"
)

func TestJamSharedQueueAndGuestControls(t *testing.T) {
	host := &jamMember{ID: "host", Name: "Andy"}
	guest := &jamMember{ID: "guest", Name: "Friend"}
	s := &jamSession{HostID: host.ID, Members: map[string]*jamMember{
		host.ID: host, guest.ID: guest,
	}, Queue: []jamQueueItem{}}
	h := &jamHub{}
	command := func(member *jamMember, c jamCommand, want string) {
		t.Helper()
		if got := h.commandLocked(s, member, c); got != want {
			t.Fatalf("%s: got %q, want %q", c.Type, got, want)
		}
	}
	command(guest, jamCommand{Type: "playback.next"}, "host_only")
	on := true
	command(guest, jamCommand{Type: "guest_controls.set", GuestPlayback: &on}, "host_only")
	command(host, jamCommand{Type: "guest_controls.set", GuestPlayback: &on}, "")
	command(guest, jamCommand{Type: "playback.set", TrackID: "song-1", Playing: &on,
		PositionMS: new(int64)}, "host_only")
	command(host, jamCommand{Type: "queue.add", TrackID: "song-2"}, "")
	command(guest, jamCommand{Type: "queue.add", TrackID: "song-3", Next: true}, "")
	if s.Queue[0].TrackID != "song-3" {
		t.Fatal("play next must lead the queue")
	}
	last := 1
	command(guest, jamCommand{Type: "queue.move", ItemID: s.Queue[0].ID, ToIndex: &last}, "")
	if s.Queue[1].TrackID != "song-3" {
		t.Fatal("guests must be able to reorder")
	}
	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	command(guest, jamCommand{Type: "profile.set", Avatar: base64.StdEncoding.EncodeToString(picture.Bytes())}, "")
	if guest.Avatar == "" || s.snapshot()["guest_playback"] != true {
		t.Fatal("profile or controls missing from state")
	}
	command(guest, jamCommand{Type: "profile.set", Avatar: "invalid"}, "invalid_avatar")
}

func TestJamHostAuthorityAndTransportRequests(t *testing.T) {
	host, guest := &jamMember{ID: "host"}, &jamMember{ID: "guest"}
	hostClient := &jamClient{send: make(chan []byte, 20), cancel: func() {}}
	guestClient := &jamClient{send: make(chan []byte, 20), cancel: func() {}}
	s := &jamSession{HostID: host.ID, GuestPlayback: true, Revision: 4,
		Queue:    []jamQueueItem{{ID: "queued", TrackID: "next", Votes: map[string]bool{guest.ID: true}}},
		Playback: jamPlayback{TrackID: "current", Playing: true, PositionMS: 1200},
		clients:  map[*jamClient]string{hostClient: host.ID, guestClient: guest.ID}}
	h := &jamHub{}
	on, repeat, position := true, 2, int64(4500)
	for _, action := range []string{"play", "pause", "next", "previous", "shuffle", "repeat", "seek", "track", "queue_item"} {
		c := jamCommand{Type: "playback.request", Action: action, Shuffle: &on, Repeat: &repeat, PositionMS: &position, TrackID: "requested", ItemID: "queued"}
		if got := h.commandLocked(s, guest, c); got != "" {
			t.Fatalf("%s: %s", action, got)
		}
		if s.Revision != 4 || s.Playback.TrackID != "current" || len(s.Queue) != 1 || len(guestClient.send) != 0 {
			t.Fatal("guest command changed playback before the host applied it")
		}
		var event struct {
			Type    string
			Command jamCommand
		}
		if err := json.Unmarshal(<-hostClient.send, &event); err != nil || event.Type != "playback.request" || event.Command.Action != action {
			t.Fatal("request must reach the host once")
		}
	}
	s.GuestPlayback = false
	if h.commandLocked(s, guest, jamCommand{Type: "playback.request", Action: "previous"}) != "host_only" {
		t.Fatal("host switch must apply to every control")
	}
	s.GuestPlayback = true
	delete(s.clients, hostClient)
	if h.commandLocked(s, guest, jamCommand{Type: "playback.request", Action: "next"}) != "host_unavailable" {
		t.Fatal("offline host must not advance playback")
	}
	if h.commandLocked(s, guest, jamCommand{Type: "playback.set", TrackID: "next", Playing: &on, PositionMS: &position}) != "host_only" {
		t.Fatal("only host may confirm playback")
	}
	for _, c := range []jamCommand{
		{Type: "playback.request", Action: "seek", PositionMS: new(int64(-1))},
		{Type: "playback.request", Action: "shuffle"},
		{Type: "playback.request", Action: "repeat", Repeat: new(3)},
		{Type: "playback.request", Action: "track", TrackID: "bad id"},
	} {
		if h.commandLocked(s, guest, c) == "" {
			t.Fatal("invalid control accepted")
		}
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "playback.set", ItemID: "queued", TrackID: "wrong", Playing: &on, PositionMS: &position}); got != "item_not_found" || len(s.Queue) != 1 {
		t.Fatal("mismatched consumption changed queue")
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "playback.set", ItemID: "queued", TrackID: "next", Playing: &on, PositionMS: &position, Shuffle: &on, Repeat: &repeat}); got != "" {
		t.Fatal(got)
	}
	if len(s.Queue) != 0 || s.Playback.TrackID != "next" || !s.Playback.Shuffle || s.Playback.Repeat != 2 {
		t.Fatal("host confirmation must atomically publish modes and consume the played entry")
	}
}

func TestJamShuffleAndPreviousPreserveQueueVotes(t *testing.T) {
	host, guest := &jamMember{ID: "host"}, &jamMember{ID: "guest"}
	s := &jamSession{HostID: host.ID, Queue: []jamQueueItem{
		{ID: "first", TrackID: "one", Votes: map[string]bool{"guest": true}},
		{ID: "second", TrackID: "two", Votes: map[string]bool{}},
	}}
	h := &jamHub{}
	on, position := true, int64(0)
	if got := h.commandLocked(s, host, jamCommand{Type: "playback.set", TrackID: "one", ItemID: "first", Playing: &on, PositionMS: &position, QueueIDs: []string{"invalid"}}); got != "invalid_queue_order" || len(s.Queue) != 2 || s.Playback.TrackID != "" {
		t.Fatal("invalid confirmation must leave both playback and queue unchanged")
	}
	if h.commandLocked(s, guest, jamCommand{Type: "queue.order", ItemIDs: []string{"second", "first"}}) != "host_only" {
		t.Fatal("only host can confirm shuffle order")
	}
	if h.commandLocked(s, host, jamCommand{Type: "queue.order", ItemIDs: []string{"second", "second"}}) != "invalid_queue_order" || s.Queue[0].ID != "first" {
		t.Fatal("invalid shuffle order changed queue")
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "queue.order", ItemIDs: []string{"second", "first"}}); got != "" {
		t.Fatal(got)
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "queue.restore", TrackIDs: []string{"played-before", "two", "one"}}); got != "" {
		t.Fatal(got)
	}
	if len(s.Queue) != 3 || s.Queue[1].ID != "second" || s.Queue[2].ID != "first" || !s.Queue[2].Votes["guest"] {
		t.Fatal("previous/repeat must retain upcoming identities and votes")
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "playback.set", TrackID: "two", ItemID: "second", ItemIDs: []string{s.Queue[0].ID}, Playing: &on, PositionMS: &position, QueueIDs: []string{"first"}}); got != "" || len(s.Queue) != 1 || s.Queue[0].ID != "first" {
		t.Fatal("rapid skips must consume passed entries and publish remaining order atomically")
	}
}

func TestJamSeedPreservesOrder(t *testing.T) {
	host := &jamMember{ID: "host"}
	guest := &jamMember{ID: "guest"}
	s := &jamSession{HostID: host.ID, Queue: []jamQueueItem{}}
	h := &jamHub{}
	ids := make([]string, 543)
	for i := range ids {
		ids[i] = "track-" + strconv.Itoa(i)
	}
	if got := h.commandLocked(s, guest, jamCommand{Type: "queue.seed", TrackIDs: ids}); got != "host_only" {
		t.Fatalf("guest seed: %s", got)
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "queue.seed", TrackIDs: ids}); got != "" {
		t.Fatalf("host seed: %s", got)
	}
	for i, id := range ids {
		if s.Queue[i].TrackID != id || s.Queue[i].AddedBy != host.ID {
			t.Fatal("seed changed playback order or owner")
		}
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "queue.seed", TrackIDs: ids}); got != "queue_limit" {
		t.Fatalf("seed must not duplicate on reconnect: %s", got)
	}
	if got := h.commandLocked(s, guest, jamCommand{Type: "queue.clear"}); got != "host_only" || len(s.Queue) != len(ids) {
		t.Fatal("guest must not clear the queue")
	}
	if got := h.commandLocked(s, host, jamCommand{Type: "queue.clear"}); got != "" || len(s.Queue) != 0 {
		t.Fatal("host clear must empty the queue")
	}
}
