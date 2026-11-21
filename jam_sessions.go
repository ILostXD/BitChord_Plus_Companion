package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type jamHub struct {
	// ponytail: bounded, in-memory sessions with one lock; add persistence/per-session locks when needed.
	mu                sync.Mutex
	sessions          map[string]*jamSession
	cfg               config
	contributionBytes int64
}

type jamMember struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Avatar    string `json:"avatar,omitempty"`
	token     string
	streams   int
	uploading bool
}

type jamPlayback struct {
	TrackID    string `json:"track_id"`
	Playing    bool   `json:"playing"`
	PositionMS int64  `json:"position_ms"`
	UpdatedMS  int64  `json:"updated_at_ms"`
	Shuffle    bool   `json:"shuffle"`
	Repeat     int    `json:"repeat"`
}

type jamQueueItem struct {
	ID      string          `json:"id"`
	TrackID string          `json:"track_id"`
	AddedBy string          `json:"added_by"`
	Votes   map[string]bool `json:"votes"`
	Queued  bool            `json:"queued,omitempty"`
	order   uint64
}

type jamSession struct {
	ID              string
	InviteToken     string
	HostID          string
	Revision        uint64
	Expires         time.Time
	Members         map[string]*jamMember
	Queue           []jamQueueItem
	Playback        jamPlayback
	GuestPlayback   bool
	clients         map[*jamClient]string
	libraryIdentity navidromeIdentity // Memory-only host credential, never serialized in snapshots.
	artwork         map[string]bool
	searchTracks    map[string]bool
	contributions   map[string]*jamContribution
}

func jamID(bytes int) string {
	data := make([]byte, bytes)
	// crypto/rand.Read cannot fail on supported Go platforms.
	_, _ = rand.Read(data)
	return hex.EncodeToString(data)
}

func (s *jamSession) snapshot() map[string]any {
	members := make([]jamMember, 0, len(s.Members))
	for _, member := range s.Members {
		members = append(members, jamMember{ID: member.ID, Name: member.Name, Avatar: member.Avatar})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	queue := make([]jamQueueItem, len(s.Queue))
	for i, item := range s.Queue {
		queue[i] = item
		queue[i].Votes = maps.Clone(item.Votes)
	}
	return map[string]any{
		"id": s.ID, "host_id": s.HostID, "revision": s.Revision,
		"expires_at": s.Expires.UTC().Format(time.RFC3339), "members": members,
		"queue": queue, "playback": s.Playback, "guest_playback": s.GuestPlayback,
		"server_time_ms": time.Now().UnixMilli(),
	}
}

func jamError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func jamName(w http.ResponseWriter, r *http.Request, joining bool) (string, string, bool) {
	var body struct {
		Name        string `json:"name"`
		InviteToken string `json:"invite_token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		jamError(w, 400, "invalid_json")
		return "", "", false
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 80 || strings.ContainsFunc(name, unicode.IsControl) {
		jamError(w, 400, "invalid_name")
		return "", "", false
	}
	if joining && len(body.InviteToken) != 64 {
		jamError(w, 403, "invalid_invite")
		return "", "", false
	}
	return name, body.InviteToken, true
}

func (h *jamHub) expireLocked() {
	for id, session := range h.sessions {
		if time.Now().After(session.Expires) {
			h.endLocked(session, "expired")
			delete(h.sessions, id)
		}
	}
}

func (h *jamHub) memberLocked(w http.ResponseWriter, r *http.Request, id string) (*jamSession, *jamMember) {
	h.expireLocked()
	session := h.sessions[id]
	if session == nil {
		jamError(w, 404, "session_not_found")
		return nil, nil
	}
	token := r.Header.Get("X-Jam-Member-Token")
	for _, member := range session.Members {
		if subtle.ConstantTimeCompare([]byte(token), []byte(member.token)) == 1 {
			return session, member
		}
	}
	jamError(w, 403, "invalid_member_token")
	return nil, nil
}

func (h *jamHub) registerREST(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/jam/sessions", func(w http.ResponseWriter, r *http.Request) {
		name, _, ok := jamName(w, r, false)
		if !ok {
			return
		}
		h.mu.Lock()
		h.expireLocked()
		if len(h.sessions) >= 64 {
			h.mu.Unlock()
			jamError(w, 429, "session_limit")
			return
		}
		member := &jamMember{ID: jamID(12), Name: name, token: jamID(32)}
		session := &jamSession{ID: jamID(16), InviteToken: jamID(32), HostID: member.ID, Revision: 1,
			GuestPlayback: true,
			Expires:       time.Now().Add(24 * time.Hour), Members: map[string]*jamMember{member.ID: member},
			Queue: []jamQueueItem{}, clients: make(map[*jamClient]string),
			Playback: jamPlayback{UpdatedMS: time.Now().UnixMilli()}}
		session.libraryIdentity, _ = r.Context().Value(navidromeIdentityKey{}).(navidromeIdentity)
		session.artwork, session.searchTracks = make(map[string]bool), make(map[string]bool)
		h.sessions[session.ID] = session
		body := map[string]any{"session": session.snapshot(), "member_id": member.ID, "member_token": member.token, "invite_token": session.InviteToken}
		h.mu.Unlock()
		writeJSON(w, 201, body)
	})
	mux.HandleFunc("POST /api/v1/jam/sessions/{sessionId}/join", func(w http.ResponseWriter, r *http.Request) {
		name, invite, ok := jamName(w, r, true)
		if !ok {
			return
		}
		h.mu.Lock()
		h.expireLocked()
		session := h.sessions[r.PathValue("sessionId")]
		if session == nil {
			h.mu.Unlock()
			jamError(w, 404, "session_not_found")
			return
		}
		if subtle.ConstantTimeCompare([]byte(invite), []byte(session.InviteToken)) != 1 {
			h.mu.Unlock()
			jamError(w, 403, "invalid_invite")
			return
		}
		if len(session.Members) >= 32 {
			h.mu.Unlock()
			jamError(w, 429, "member_limit")
			return
		}
		member := &jamMember{ID: jamID(12), Name: name, token: jamID(32)}
		session.Members[member.ID] = member
		h.changedLocked(session)
		body := map[string]any{"session": session.snapshot(), "member_id": member.ID, "member_token": member.token}
		h.mu.Unlock()
		writeJSON(w, 201, body)
	})
	mux.HandleFunc("GET /api/v1/jam/sessions/{sessionId}", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		session, _ := h.memberLocked(w, r, r.PathValue("sessionId"))
		if session == nil {
			h.mu.Unlock()
			return
		}
		body := session.snapshot()
		h.mu.Unlock()
		writeJSON(w, 200, body)
	})
	for _, pattern := range []string{"DELETE /api/v1/jam/sessions/{sessionId}", "POST /api/v1/jam/sessions/{sessionId}/leave"} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			h.mu.Lock()
			defer h.mu.Unlock()
			session, member := h.memberLocked(w, r, r.PathValue("sessionId"))
			if session == nil {
				return
			}
			if r.Method == "DELETE" && member.ID != session.HostID {
				jamError(w, 403, "host_only")
				return
			}
			if member.ID == session.HostID {
				h.endLocked(session, "host_left")
				delete(h.sessions, session.ID)
			} else {
				delete(session.Members, member.ID)
				h.removeContributionsLocked(session, member.ID)
				for i := range session.Queue {
					delete(session.Queue[i].Votes, member.ID)
				}
				for client, id := range session.clients {
					if id == member.ID {
						client.cancel()
						delete(session.clients, client)
					}
				}
				session.sortQueue()
				h.changedLocked(session)
			}
			writeJSON(w, 200, map[string]string{"status": "left"})
		})
	}
}
