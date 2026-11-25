// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package server

import (
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

// historyOf is what the history holds of a user (tag): hourly and daily buckets and addresses.
func historyOf(m *Monitor, tag string) (n int) {
	m.readHistory(func(h *History) { n = len(h.Hourly[tag]) + len(h.Daily[tag]) + len(h.Clients[tag]) })
	return n
}

// A user deleted while a flush is under way does not come back into the history. The flush reads the
// user's counters; the user is deleted; the flush writes what it read. Before, the deletion could
// happen between the reading and the writing, because the flush let go of the counters before it
// took the history, and the write then brought the deleted user's traffic and addresses back.
func TestForgetWhileAFlushIsUnderWayLeavesNoHistory(t *testing.T) {
	m := NewMonitor()
	const tag = "u9"
	now := time.Now()
	m.AddExternal(tag, IKEv2, 100, 200, "198.51.100.30", now)
	forgotten := make(chan struct{})
	m.afterFlushRead = func() {
		go func() {
			m.Forget(tag)
			close(forgotten)
		}()
		// Before, the deletion went through here at once. Now it waits for the flush to end.
		select {
		case <-forgotten:
		case <-time.After(300 * time.Millisecond):
		}
	}
	m.Flush(now)
	select {
	case <-forgotten:
	case <-time.After(5 * time.Second):
		t.Fatal("the deletion did not finish")
	}
	if n := historyOf(m, tag); n != 0 {
		t.Fatalf("%d history record(s) of a deleted user", n)
	}
	if users, _ := m.Live(); users[tag] != (UserLive{}) {
		t.Fatalf("a deleted user still has live counters: %+v", users[tag])
	}
	// What the flush read was written before the deletion, so a flush after it has nothing to add.
	m.afterFlushRead = nil
	m.Flush(now)
	if n := historyOf(m, tag); n != 0 {
		t.Fatalf("%d history record(s) of a deleted user after another flush", n)
	}
}

// Flushes and deletions at once leave no history of a user that was deleted.
func TestFlushesAndDeletionsAtOnceLeaveNoHistoryOfDeletedUsers(t *testing.T) {
	m := NewMonitor()
	now := time.Now()
	stop := make(chan struct{})
	var flushers sync.WaitGroup
	for i := 0; i < 3; i++ {
		flushers.Add(1)
		go func() {
			defer flushers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					m.Flush(now)
				}
			}
		}()
	}
	const users = 400
	for i := 0; i < users; i++ {
		tag := fmt.Sprintf("u%d", i)
		m.AddExternal(tag, IKEv2, 10, 20, "198.51.100.30", now)
		runtime.Gosched()
		m.Forget(tag)
	}
	close(stop)
	flushers.Wait()
	m.Flush(now)
	for i := 0; i < users; i++ {
		if n := historyOf(m, fmt.Sprintf("u%d", i)); n != 0 {
			t.Fatalf("u%d was deleted and has %d history record(s)", i, n)
		}
	}
}

// A deleted client's last counters and addresses are not written back into the history.
func TestDeletedClientsLeaveNoHistory(t *testing.T) {
	server := newTestServer(t, nil)
	user := server.user(t, map[string]any{"name": "Gone"})
	tag := "u" + user.ID
	server.service.monitor.AddExternal(tag, IKEv2, 100, 200, "198.51.100.30", time.Now())
	server.must(t, http.StatusNoContent, "DELETE", "/users/"+user.ID, nil, nil)
	server.service.Record(time.Now())
	server.service.monitor.readHistory(func(h *History) {
		if len(h.Hourly[tag])+len(h.Daily[tag])+len(h.Clients[tag]) != 0 {
			t.Fatalf("history of a deleted client: %v %v %v", h.Hourly[tag], h.Daily[tag], h.Clients[tag])
		}
	})
	if users, _ := server.service.monitor.Live(); users[tag] != (UserLive{}) {
		t.Fatalf("a deleted client still has live counters: %+v", users[tag])
	}
}

// history_days below the default site retention needs no site setting of its own, and sizes
// are in MB of 1,000,000 bytes like everywhere else.
func TestShortHistoryNeedsNoSiteSetting(t *testing.T) {
	server := newTestServer(t, nil)
	var info serverInfo
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"history_days": 14}, &info)
	if info.HistoryDays != 14 || info.SiteDays != 14 {
		t.Fatalf("history %d, sites %d", info.HistoryDays, info.SiteDays)
	}
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"history_days": 60, "site_days": 30}, &info)
	if status := server.call(t, "PATCH", "/server", map[string]any{"history_days": 14}, nil); status != http.StatusBadRequest {
		t.Fatalf("a site retention set longer than the history: status %d", status)
	}
	if got := (MonitorSettings{}).historyMaxBytes(); got != 20_000_000 || info.HistoryMaxMB != 20 {
		t.Fatalf("the default history cap is %d bytes (%d MB), want 20 MB", got, info.HistoryMaxMB)
	}
}
