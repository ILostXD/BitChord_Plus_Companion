package main

import (
	"reflect"
	"testing"
)

func TestJamManualQueueOrderAndShuffleSurviveEverySkip(t *testing.T) {
	for _, insertion := range []string{"before", "after"} {
		t.Run(insertion, func(t *testing.T) {
			host, guest := &jamMember{ID: "host"}, &jamMember{ID: "guest"}
			s := &jamSession{HostID: host.ID, Members: map[string]*jamMember{"host": host, "guest": guest}}
			h := &jamHub{}
			call := func(member *jamMember, c jamCommand) {
				t.Helper()
				if err := h.commandLocked(s, member, c); err != "" {
					t.Fatal(c.Type, err)
				}
			}
			call(host, jamCommand{Type: "queue.seed", TrackIDs: []string{"p1", "p2", "p3", "p4", "p5"}})
			call(guest, jamCommand{Type: "queue.add", TrackID: "A", Insertion: insertion})
			call(host, jamCommand{Type: "queue.add", TrackID: "B", Insertion: insertion})
			want := []string{"A", "B", "p1", "p2", "p3", "p4", "p5"}
			if insertion == "before" {
				want[0], want[1] = "B", "A"
			}
			tracks := func() []string {
				out := []string{}
				for _, item := range s.Queue {
					out = append(out, item.TrackID)
				}
				return out
			}
			if !reflect.DeepEqual(tracks(), want) {
				t.Fatal(tracks(), want)
			}
			before := len(s.Queue)
			if err := h.commandLocked(s, guest, jamCommand{Type: "queue.add", TrackID: "bad", Insertion: "invalid"}); err != "invalid_insertion" || len(s.Queue) != before {
				t.Fatal("invalid insertion mutated queue")
			}
			order := []string{}
			for i := len(s.Queue) - 1; i >= 0; i-- {
				order = append(order, s.Queue[i].ID)
			}
			call(host, jamCommand{Type: "queue.order", ItemIDs: order})
			for len(s.Queue) > 0 {
				head := s.Queue[0]
				remaining := []string{}
				for _, item := range s.Queue[1:] {
					remaining = append(remaining, item.ID)
				}
				playing, shuffle, position := true, true, int64(0)
				call(host, jamCommand{Type: "playback.set", TrackID: head.TrackID, ItemID: head.ID,
					QueueIDs: remaining, Playing: &playing, Shuffle: &shuffle, PositionMS: &position})
				if !s.Playback.Shuffle || s.Playback.TrackID != head.TrackID {
					t.Fatal("lost playback state")
				}
				for i, item := range s.Queue {
					if item.ID != remaining[i] {
						t.Fatal("shuffle reverted after skip")
					}
				}
			}
		})
	}
}
