// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package ipsecagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/strongswan/govici/vici"

	"ghostlink.local/outline-worker/internal/hostfw/hostfwtest"
	"ghostlink.local/outline-worker/internal/ipsecproto"
)

// firewallAgent is an agent whose settings are applied to a pretend host, as after a successful
// apply: the rules are in, and the last check of the jumps has just happened.
func firewallAgent(t *testing.T) (*Agent, *hostfwtest.Host, *bytes.Buffer) {
	t.Helper()
	host := hostfwtest.New()
	logs := &bytes.Buffer{}
	a := &Agent{Dir: t.TempDir(), Run: t.TempDir(), Log: log.New(logs, "", 0), FW: &Firewall{Backend: "nft", Exec: host.Exec},
		list: func(context.Context) ([]ipsecproto.SA, error) { return nil, nil }}
	d := fixture(t)
	if err := a.FW.Apply(context.Background(), d, nil); err != nil {
		t.Fatal(err)
	}
	a.desired, a.applied, a.lastJumps = d, d.Generation, time.Now()
	return a, host, logs
}

func statusOf(t *testing.T, a *Agent) ipsecproto.Status {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(a.Dir, ipsecproto.StatusFile))
	if err != nil {
		t.Fatal(err)
	}
	var status ipsecproto.Status
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	return status
}

// wholeFirewall fails the test unless the host holds exactly the rules the agent renders for d,
// jumped to from the built-in chains.
func wholeFirewall(t *testing.T, a *Agent, host *hostfwtest.Host) {
	t.Helper()
	want := hostfwtest.New()
	reference := &Firewall{Backend: "nft", Exec: want.Exec}
	if err := reference.Apply(context.Background(), a.desired, nil); err != nil {
		t.Fatal(err)
	}
	for table, names := range chains {
		for _, name := range names {
			if got, wanted := host.Rules(table, name), want.Rules(table, name); !slices.Equal(got, wanted) || !host.Has(table, name) {
				t.Errorf("%s/%s holds %d rules, want %d", table, name, len(got), len(wanted))
			}
		}
	}
	for _, j := range jumps {
		if !slices.Contains(host.Rules(j[0], j[1]), "-j "+j[2]) {
			t.Errorf("%s/%s does not jump to %s", j[0], j[1], j[2])
		}
	}
	if missing := a.FW.Missing(context.Background()); len(missing) != 0 {
		t.Errorf("missing: %v", missing)
	}
}

// After a firewall reload the agent puts all of its firewall back, not only its jumps: with the
// chains empty the jumps would send the clients' packets through nothing (no isolation of the
// server's networks, no NAT, no DNS redirect). It does so at its next check, and only once.
func TestFlushedFirewallIsRestoredAtTheNextCheck(t *testing.T) {
	ctx := context.Background()
	for name, reload := range map[string]func(*hostfwtest.Host){
		"iptables -F (rules flushed, chains stay)": (*hostfwtest.Host).Flush,
		"a reload that replaces every table":       (*hostfwtest.Host).Reset,
	} {
		t.Run(name, func(t *testing.T) {
			a, host, logs := firewallAgent(t)
			wholeFirewall(t, a, host)
			reload(host)
			if len(host.Rules("filter", "GHOSTLINK-IPSEC-OUT")) != 0 {
				t.Fatal("the reload left the isolation rules")
			}

			// Within 30 seconds of the last check nothing happens yet.
			a.cycle(ctx, true)
			if len(host.Rules("filter", "GHOSTLINK-IPSEC-OUT")) != 0 {
				t.Fatal("the agent checked again before 30 seconds passed")
			}

			a.lastJumps = time.Now().Add(-31 * time.Second)
			a.cycle(ctx, true)
			wholeFirewall(t, a, host)
			if status := statusOf(t, a); status.Error != "" || !status.Running {
				t.Fatalf("status after the repair: %+v", status)
			}
			if !strings.Contains(logs.String(), "restoring them") || !strings.Contains(logs.String(), "restored the IPsec firewall rules") {
				t.Errorf("the repair is not in the log:\n%s", logs)
			}

			// Once it is whole again, nothing more is applied.
			restores, changes := host.Count("-restore"), host.Count(" -I ")
			for i := 0; i < 3; i++ {
				a.lastJumps = time.Now().Add(-31 * time.Second)
				a.cycle(ctx, true)
			}
			if host.Count("-restore") != restores || host.Count(" -I ") != changes {
				t.Fatalf("an intact firewall was applied again: %d restores (was %d), %d inserts (was %d)",
					host.Count("-restore"), restores, host.Count(" -I "), changes)
			}
		})
	}
}

// One lost jump is enough: the rule set is applied again as a whole.
func TestOneLostJumpRestoresTheWholeFirewall(t *testing.T) {
	a, host, _ := firewallAgent(t)
	if !host.Delete("nat", "PREROUTING", "-j GHOSTLINK-IPSEC-PRE") {
		t.Fatal("the pretend host has no such jump")
	}
	// The chain's rules are changed behind the agent's back too: restoring only the jump would keep them.
	host.Delete("filter", "GHOSTLINK-IPSEC-OUT", "-j ACCEPT")
	a.lastJumps = time.Now().Add(-31 * time.Second)
	a.cycle(context.Background(), true)
	wholeFirewall(t, a, host)
}

// What cannot be put back shows in the status, so the administrator sees that the isolation is
// gone; the agent tries again at each check, and the message goes when it succeeds.
func TestFailedRepairShowsInTheStatus(t *testing.T) {
	ctx := context.Background()
	a, host, logs := firewallAgent(t)
	host.Flush()
	host.Fail(func(name string, args []string) error {
		if strings.HasSuffix(name, "-restore") {
			return errors.New("iptables-restore: xtables lock is held by another program")
		}
		return nil
	})
	a.lastJumps = time.Now().Add(-31 * time.Second)
	a.cycle(ctx, true)
	status := statusOf(t, a)
	if !strings.Contains(status.Error, "could not be restored") || !strings.Contains(status.Error, "xtables lock") || !status.Running {
		t.Fatalf("status.json does not report the failed repair: %+v", status)
	}
	if !strings.Contains(logs.String(), "could not be restored") {
		t.Errorf("the failure is not in the log:\n%s", logs)
	}

	// The next check, 30 seconds later, tries again and fails the same way: no hammering before that.
	attempts := host.Count("-restore")
	a.cycle(ctx, true)
	if host.Count("-restore") != attempts {
		t.Fatal("the agent retried the repair before 30 seconds passed")
	}
	a.lastJumps = time.Now().Add(-31 * time.Second)
	a.cycle(ctx, true)
	if host.Count("-restore") == attempts || !strings.Contains(statusOf(t, a).Error, "could not be restored") {
		t.Fatalf("no second attempt, or the failure left the status: %+v", statusOf(t, a))
	}

	// Once iptables works, the repair succeeds and the message goes.
	host.Fail(nil)
	a.lastJumps = time.Now().Add(-31 * time.Second)
	a.cycle(ctx, true)
	if status := statusOf(t, a); status.Error != "" {
		t.Fatalf("the status still reports a repaired firewall: %q", status.Error)
	}
	wholeFirewall(t, a, host)
}

// Checking costs only reads, and an intact firewall is never applied again, however often it is
// checked (no flapping), whatever else the status reports.
func TestIntactFirewallIsOnlyChecked(t *testing.T) {
	ctx := context.Background()
	a, host, _ := firewallAgent(t)
	before := len(host.Calls())
	for i := 0; i < 5; i++ {
		a.lastJumps = time.Now().Add(-31 * time.Second)
		a.cycle(ctx, true)
	}
	calls := host.Calls()[before:]
	if len(calls) == 0 {
		t.Fatal("the firewall was never checked")
	}
	for _, call := range calls {
		if !strings.Contains(call, " -C ") && !strings.Contains(call, " -S ") {
			t.Fatalf("a check of an intact firewall ran %q", call)
		}
	}
	if status := statusOf(t, a); status.Error != "" {
		t.Fatalf("an intact firewall reports %q", status.Error)
	}
}

// The agent does not touch the firewall before settings were applied once: there is nothing to
// restore yet, and the rules would be those of no settings.
func TestNoRepairBeforeSettingsWereApplied(t *testing.T) {
	a, host, _ := firewallAgent(t)
	host.Reset()
	a.applied, a.lastJumps = 0, time.Time{}
	a.cycle(context.Background(), true)
	if host.Count("-restore") != 3 { // the three tables of the firewallAgent's own first apply
		t.Fatalf("the agent applied a firewall for settings it never applied: %v", host.Calls())
	}
}

// Events from charon are recorded but do not run a cycle or count as a tick, so the full cycle
// (sessions listed, firewall checked, status written) still runs once in fullEvery ticks of the
// clock, however many events come.
func TestEventsDoNotMoveTheFullCycle(t *testing.T) {
	a := &Agent{Log: log.New(&bytes.Buffer{}, "", 0), events: make(chan vici.Event, 256), closed: map[string]closedChild{}}
	tick := make(chan time.Time)
	cycles := make(chan bool, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- a.loop(ctx, tick, func(_ context.Context, full bool) { cycles <- full })
	}()
	next := func() bool {
		t.Helper()
		select {
		case full := <-cycles:
			return full
		case <-time.After(5 * time.Second):
			t.Fatal("the cycle did not run")
			return false
		}
	}
	var got []bool
	got = append(got, next()) // the first cycle runs at once and is a full one
	for second := 0; second < 6; second++ {
		// A burst of charon events between two ticks.
		for i := 0; i < 20; i++ {
			a.events <- vici.Event{}
		}
		for deadline := time.Now().Add(5 * time.Second); len(a.events) > 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the loop does not read charon's events")
			}
		}
		select {
		case tick <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatal("the loop does not take the tick")
		}
		got = append(got, next())
		if extra := len(cycles); extra != 0 {
			t.Fatalf("%d more cycles ran for the 20 events of second %d: an event is not a tick", extra, second+1)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if want := []bool{true, false, false, false, false, true, false}; !slices.Equal(got, want) {
		t.Fatalf("full cycles %v, want %v: events must not count as ticks", got, want)
	}
}

// A closed event channel (charon's session ended) stops being listened to without ending the
// loop, which goes on with the clock.
func TestLoopSurvivesAClosedEventChannel(t *testing.T) {
	a := &Agent{Log: log.New(&bytes.Buffer{}, "", 0), events: make(chan vici.Event)}
	close(a.events)
	tick := make(chan time.Time)
	cycles := make(chan bool, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.loop(ctx, tick, func(_ context.Context, full bool) { cycles <- full }) }()
	<-cycles
	tick <- time.Now()
	select {
	case <-cycles:
	case <-time.After(5 * time.Second):
		t.Fatal("no cycle after the event channel closed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
