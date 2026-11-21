package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	_ "image/jpeg"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

type jamClient struct {
	send   chan []byte
	cancel context.CancelFunc
}

// All sends happen under the hub lock. A slow client reconnects for a fresh snapshot.
func (c *jamClient) enqueue(body any) bool {
	data, _ := json.Marshal(body)
	return c.enqueueBytes(data)
}

func (c *jamClient) enqueueBytes(data []byte) bool {
	select {
	case c.send <- data:
		return true
	default:
		c.cancel()
		return false
	}
}

func (s *jamSession) sortQueue() {
	sort.SliceStable(s.Queue, func(i, j int) bool {
		if len(s.Queue[i].Votes) == len(s.Queue[j].Votes) {
			return s.Queue[i].order < s.Queue[j].order
		}
		return len(s.Queue[i].Votes) > len(s.Queue[j].Votes)
	})
}

func (h *jamHub) changedLocked(s *jamSession) {
	s.Revision++
	event, _ := json.Marshal(map[string]any{"type": "state", "session": s.snapshot()})
	for client := range s.clients {
		if !client.enqueueBytes(event) {
			delete(s.clients, client)
		}
	}
}

func (h *jamHub) endLocked(s *jamSession, reason string) {
	h.removeContributionsLocked(s, "")
	for client := range s.clients {
		client.enqueue(map[string]string{"type": "session_ended", "reason": reason})
		close(client.send)
		delete(s.clients, client)
	}
}

func (h *jamHub) socket(root context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session_id")
		h.mu.Lock()
		session, member := h.memberLocked(w, r, id)
		if session == nil {
			h.mu.Unlock()
			return
		}
		connections := 0
		for _, memberID := range session.clients {
			if memberID == member.ID {
				connections++
			}
		}
		if connections >= 4 {
			h.mu.Unlock()
			jamError(w, 429, "connection_limit")
			return
		}
		// Reserve before upgrading, so concurrent dials cannot bypass the limit.
		ctx, cancel := context.WithCancel(root)
		client := &jamClient{send: make(chan []byte, 16), cancel: cancel}
		session.clients[client] = member.ID
		client.enqueue(map[string]any{"type": "connected", "member_id": member.ID, "session": session.snapshot()})
		h.mu.Unlock()
		defer func() {
			cancel()
			h.mu.Lock()
			delete(session.clients, client)
			h.mu.Unlock()
		}()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(512 * 1024)
		slog.Info("jam_connected")
		defer slog.Info("jam_disconnected")
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			defer cancel()
			for {
				select {
				case <-ctx.Done():
					return
				case data, ok := <-client.send:
					if !ok {
						_ = conn.Close(websocket.StatusNormalClosure, "Session ended")
						return
					}
					writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
					err := conn.Write(writeCtx, websocket.MessageText, data)
					stop()
					if err != nil {
						return
					}
				}
			}
		}()
		defer func() { cancel(); <-writerDone }()
		// Bound command bursts; heartbeats count too. No dependency or background timer needed.
		budget, lastMessage := 40.0, time.Now()
		for {
			readCtx, stop := context.WithTimeout(ctx, 75*time.Second)
			kind, data, err := conn.Read(readCtx)
			stop()
			if err != nil {
				return
			}
			if kind != websocket.MessageText {
				_ = conn.Close(websocket.StatusUnsupportedData, "JSON text only")
				return
			}
			now := time.Now()
			budget = min(40, budget+now.Sub(lastMessage).Seconds()*20)
			lastMessage = now
			if budget < 1 {
				_ = conn.Close(websocket.StatusPolicyViolation, "Message rate exceeded")
				return
			}
			budget--
			var message jamCommand
			if json.Unmarshal(data, &message) != nil {
				_ = conn.Close(websocket.StatusInvalidFramePayloadData, "Invalid JSON")
				return
			}
			h.mu.Lock()
			h.expireLocked()
			if h.sessions[id] != session || session.Members[member.ID] == nil {
				h.mu.Unlock()
				// Let the writer deliver a terminal event before cancelling this socket.
				select {
				case <-writerDone:
				case <-ctx.Done():
				}
				return
			}
			if message.Type == "heartbeat" {
				client.enqueue(map[string]any{"type": "heartbeat", "nonce": message.Nonce, "server_time_ms": time.Now().UnixMilli()})
			} else if code := h.commandLocked(session, member, message); code != "" {
				client.enqueue(map[string]string{"type": "error", "error": code})
			}
			h.mu.Unlock()
		}
	}
}

type jamCommand struct {
	Type             string   `json:"type"`
	Nonce            string   `json:"nonce"`
	TrackID          string   `json:"track_id"`
	TrackIDs         []string `json:"track_ids"`
	ItemID           string   `json:"item_id"`
	Vote             *bool    `json:"vote"`
	Playing          *bool    `json:"playing"`
	PositionMS       *int64   `json:"position_ms"`
	ExpectedRevision *uint64  `json:"expected_revision"`
	ToIndex          *int     `json:"to_index"`
	GuestPlayback    *bool    `json:"guest_playback"`
	Avatar           string   `json:"avatar"`
	Next             bool     `json:"next"`
	Insertion        string   `json:"insertion"`
	Action           string   `json:"action"`
	ItemIDs          []string `json:"item_ids"`
	QueueIDs         []string `json:"queue_ids"`
	Shuffle          *bool    `json:"shuffle"`
	Repeat           *int     `json:"repeat"`
}

func validTrackID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.ContainsFunc(id, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func (h *jamHub) commandLocked(s *jamSession, member *jamMember, c jamCommand) string {
	if c.ExpectedRevision != nil && *c.ExpectedRevision != s.Revision {
		return "stale_revision"
	}
	switch c.Type {
	case "queue.restore":
		if member.ID != s.HostID {
			return "host_only"
		}
		if len(c.TrackIDs) > 1000 {
			return "queue_limit"
		}
		for _, id := range c.TrackIDs {
			if !validTrackID(id) {
				return "invalid_track_id"
			}
		}
		remaining := append([]jamQueueItem(nil), s.Queue...)
		// ponytail: matching history against a queue is quadratic, bounded to 1000 entries.
		queue := make([]jamQueueItem, 0, len(c.TrackIDs))
		for i, id := range c.TrackIDs {
			item := jamQueueItem{ID: jamID(12), TrackID: id, AddedBy: member.ID, Votes: make(map[string]bool)}
			for j, existing := range remaining {
				if existing.TrackID == id {
					item = existing
					remaining = append(remaining[:j], remaining[j+1:]...)
					break
				}
			}
			item.order = uint64(i)
			queue = append(queue, item)
		}
		s.Queue = queue
	case "playback.request":
		if member.ID != s.HostID && !s.GuestPlayback {
			return "host_only"
		}
		switch c.Action {
		case "play", "pause", "next", "previous":
		case "track":
			if !validTrackID(c.TrackID) {
				return "invalid_track_id"
			}
			if member.ID != s.HostID && s.libraryIdentity.User != "" && !s.searchTracks[c.TrackID] &&
				c.TrackID != s.Playback.TrackID && !slices.ContainsFunc(s.Queue, func(item jamQueueItem) bool { return item.TrackID == c.TrackID }) {
				return "track_not_in_session"
			}
		case "queue_item":
			found := false
			for _, item := range s.Queue {
				if item.ID == c.ItemID {
					found = true
					break
				}
			}
			if !found {
				return "item_not_found"
			}
		case "seek":
			if c.PositionMS == nil || *c.PositionMS < 0 || *c.PositionMS > 604800000 {
				return "invalid_playback"
			}
		case "shuffle":
			if c.Shuffle == nil {
				return "invalid_playback"
			}
		case "repeat":
			if c.Repeat == nil || *c.Repeat < 0 || *c.Repeat > 2 {
				return "invalid_playback"
			}
		default:
			return "unknown_command"
		}
		// Deliver once, only to the host. A request never advances shared playback.
		for client, id := range s.clients {
			if id == s.HostID && client.enqueue(map[string]any{"type": "playback.request", "command": c, "member_id": member.ID}) {
				return ""
			}
		}
		return "host_unavailable"
	case "queue.order":
		if member.ID != s.HostID {
			return "host_only"
		}
		if len(c.ItemIDs) != len(s.Queue) {
			return "invalid_queue_order"
		}
		items := make(map[string]jamQueueItem, len(s.Queue))
		for _, item := range s.Queue {
			items[item.ID] = item
		}
		ordered := make([]jamQueueItem, 0, len(s.Queue))
		for i, id := range c.ItemIDs {
			item, ok := items[id]
			if !ok {
				return "invalid_queue_order"
			}
			delete(items, id)
			item.order = uint64(i)
			ordered = append(ordered, item)
		}
		s.Queue = ordered
	case "queue.seed":
		if member.ID != s.HostID {
			return "host_only"
		}
		if len(s.Queue) != 0 || len(c.TrackIDs) > 1000 {
			return "queue_limit"
		}
		for _, id := range c.TrackIDs {
			if !validTrackID(id) {
				return "invalid_track_id"
			}
		}
		for _, id := range c.TrackIDs {
			s.Queue = append(s.Queue, jamQueueItem{ID: jamID(12), TrackID: id, AddedBy: member.ID,
				Votes: make(map[string]bool), order: s.Revision + uint64(len(s.Queue))})
		}
		s.Revision += uint64(len(c.TrackIDs))
	case "queue.clear":
		if member.ID != s.HostID {
			return "host_only"
		}
		s.Queue = []jamQueueItem{}
	case "playback.set", "playback.next":
		if member.ID != s.HostID {
			if c.Type == "playback.next" {
				c.Type, c.Action = "playback.request", "next"
				return h.commandLocked(s, member, c)
			}
			return "host_only"
		}
		if c.Type == "playback.next" {
			if len(s.Queue) == 0 {
				s.Playback = jamPlayback{UpdatedMS: time.Now().UnixMilli()}
			} else {
				s.Playback = jamPlayback{TrackID: s.Queue[0].TrackID, Playing: true, UpdatedMS: time.Now().UnixMilli()}
				s.Queue = s.Queue[1:]
			}
		} else {
			if c.Repeat != nil && (*c.Repeat < 0 || *c.Repeat > 2) {
				return "invalid_playback"
			}
			if c.PositionMS == nil || c.Playing == nil || *c.PositionMS < 0 || *c.PositionMS > int64((7*24*time.Hour)/time.Millisecond) ||
				(c.TrackID == "" && (*c.Playing || *c.PositionMS != 0)) || (c.TrackID != "" && !validTrackID(c.TrackID)) {
				return "invalid_playback"
			}
			remove := make(map[string]bool)
			if len(c.ItemIDs) > 1000 {
				return "queue_limit"
			}
			for _, id := range c.ItemIDs {
				if remove[id] {
					return "invalid_queue_order"
				}
				remove[id] = true
			}
			if c.ItemID != "" {
				remove[c.ItemID] = true
			}
			found := 0
			for _, item := range s.Queue {
				if remove[item.ID] {
					if item.ID == c.ItemID && item.TrackID != c.TrackID {
						return "item_not_found"
					}
					found++
				}
			}
			if found != len(remove) {
				return "item_not_found"
			}
			queue := make([]jamQueueItem, 0, len(s.Queue)-len(remove))
			for _, item := range s.Queue {
				if !remove[item.ID] {
					queue = append(queue, item)
				}
			}
			if c.QueueIDs != nil {
				if len(c.QueueIDs) != len(queue) {
					return "invalid_queue_order"
				}
				items := make(map[string]jamQueueItem, len(queue))
				for _, item := range queue {
					items[item.ID] = item
				}
				ordered := make([]jamQueueItem, 0, len(queue))
				for i, id := range c.QueueIDs {
					item, ok := items[id]
					if !ok {
						return "invalid_queue_order"
					}
					delete(items, id)
					item.order = uint64(i)
					ordered = append(ordered, item)
				}
				queue = ordered
			}
			s.Queue = queue
			shuffle, repeat := s.Playback.Shuffle, s.Playback.Repeat
			if c.Shuffle != nil {
				shuffle = *c.Shuffle
			}
			if c.Repeat != nil {
				repeat = *c.Repeat
			}
			s.Playback = jamPlayback{TrackID: c.TrackID, Playing: *c.Playing, PositionMS: *c.PositionMS,
				UpdatedMS: time.Now().UnixMilli(), Shuffle: shuffle, Repeat: repeat}
		}
	case "queue.add":
		if !validTrackID(c.TrackID) {
			return "invalid_track_id"
		}
		if s.libraryIdentity.User != "" && member.ID != s.HostID && !s.searchTracks[c.TrackID] &&
			!slices.ContainsFunc(s.Queue, func(item jamQueueItem) bool { return item.TrackID == c.TrackID }) {
			return "track_not_in_session"
		}
		if len(s.Queue) >= 1000 {
			return "queue_limit"
		}
		if c.Insertion != "" && c.Insertion != "before" && c.Insertion != "after" {
			return "invalid_insertion"
		}
		position := len(s.Queue) // Preserve old-client append semantics.
		if c.Next || c.Insertion == "before" {
			position = 0
		} else if c.Insertion == "after" {
			position = 0
			for i, item := range s.Queue {
				if item.Queued {
					position = i + 1
				}
			}
		}
		item := jamQueueItem{ID: jamID(12), TrackID: c.TrackID, AddedBy: member.ID,
			Votes: make(map[string]bool), Queued: true}
		s.Queue = append(s.Queue, jamQueueItem{})
		copy(s.Queue[position+1:], s.Queue[position:])
		s.Queue[position] = item
		for i := range s.Queue {
			s.Queue[i].order = uint64(i)
		}

	case "queue.vote", "queue.remove", "queue.move":
		index := -1
		for i := range s.Queue {
			if s.Queue[i].ID == c.ItemID {
				index = i
				break
			}
		}
		if index < 0 {
			return "item_not_found"
		}
		item := &s.Queue[index]
		if c.Type == "queue.remove" {
			if member.ID != s.HostID && member.ID != item.AddedBy {
				return "not_item_owner"
			}
			s.Queue = append(s.Queue[:index], s.Queue[index+1:]...)
		} else if c.Type == "queue.move" {
			if c.ToIndex == nil || *c.ToIndex < 0 || *c.ToIndex >= len(s.Queue) {
				return "invalid_index"
			}
			moved := s.Queue[index]
			s.Queue = append(s.Queue[:index], s.Queue[index+1:]...)
			to := *c.ToIndex
			s.Queue = append(s.Queue, jamQueueItem{})
			copy(s.Queue[to+1:], s.Queue[to:])
			s.Queue[to] = moved
			for i := range s.Queue {
				s.Queue[i].order = uint64(i)
			}
		} else {
			if c.Vote == nil {
				return "invalid_vote"
			}
			if item.Votes[member.ID] == *c.Vote {
				return "" // Idempotent: repeating a vote never adds another vote/revision.
			}
			if *c.Vote {
				item.Votes[member.ID] = true
			} else {
				delete(item.Votes, member.ID)
			}
			s.sortQueue()
		}
	case "guest_controls.set":
		if member.ID != s.HostID {
			return "host_only"
		}
		if c.GuestPlayback == nil {
			return "invalid_guest_controls"
		}
		s.GuestPlayback = *c.GuestPlayback
	case "profile.set":
		if len(c.Avatar) > 16384 {
			return "invalid_avatar"
		}
		if c.Avatar != "" {
			data, err := base64.StdEncoding.DecodeString(c.Avatar)
			if err != nil || len(data) > 12288 {
				return "invalid_avatar"
			}
			config, format, err := image.DecodeConfig(strings.NewReader(string(data)))
			if err != nil || format != "jpeg" || config.Width > 256 || config.Height > 256 ||
				config.Width < 1 || config.Height < 1 {
				return "invalid_avatar"
			}
		}
		member.Avatar = c.Avatar
	default:
		return "unknown_command"
	}
	h.changedLocked(s)
	return ""
}
