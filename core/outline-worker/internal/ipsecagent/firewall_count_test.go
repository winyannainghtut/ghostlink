// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package ipsecagent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/hostfw"
	"ghostlink.local/outline-worker/internal/hostfw/hostfwtest"
	"ghostlink.local/outline-worker/internal/ipsecproto"
)

// checkNow makes the next cycle check the firewall, as it does every 30 seconds.
func checkNow(a *Agent) { a.lastJumps = time.Now().Add(-31 * time.Second) }

// repairs counts how often the agent put its rules back after it found them damaged.
func repairs(logs *bytes.Buffer) int { return strings.Count(logs.String(), "restoring them") }

// onlyReads fails the test when a command that changes the host ran after the first `before`.
func onlyReads(t *testing.T, host *hostfwtest.Host, before int) {
	t.Helper()
	for _, call := range host.Calls()[before:] {
		if !strings.Contains(call, " -C ") && !strings.Contains(call, " -S ") {
			t.Fatalf("a check of an intact firewall ran %q", call)
		}
	}
}

// iptables -F on one chain leaves the chain and the jump to it, so the jumps and the chains are all
// there. Each chain of the agent that holds rules is tried: the agent notices at its next check
// and puts all of its firewall back, once.
func TestOneFlushedChainWithItsJumpIsRestoredAtTheNextCheck(t *testing.T) {
	ctx := context.Background()
	for table, names := range chains {
		for _, name := range names {
			if name == chainLimit {
				continue // holds rules only while capped clients are connected (see below)
			}
			t.Run(table+"/"+name, func(t *testing.T) {
				a, host, logs := firewallAgent(t)
				wholeFirewall(t, a, host)
				host.FlushChain(table, name)
				if len(host.Rules(table, name)) != 0 {
					t.Fatal("the flush left rules")
				}
				for _, j := range jumps {
					if !slices.Contains(host.Rules(j[0], j[1]), "-j "+j[2]) {
						t.Fatalf("the flush of one chain took the jump %s/%s -> %s away", j[0], j[1], j[2])
					}
				}

				// Within 30 seconds of the last check nothing happens yet.
				a.cycle(ctx, true)
				if len(host.Rules(table, name)) != 0 {
					t.Fatal("the agent checked again before 30 seconds passed")
				}
				checkNow(a)
				a.cycle(ctx, true)
				wholeFirewall(t, a, host)
				if repairs(logs) != 1 || !strings.Contains(logs.String(), "chain "+table+"/"+name+" holds 0 of ") {
					t.Fatalf("the repair is not in the log, or not as one repair:\n%s", logs)
				}
				if status := statusOf(t, a); status.Error != "" || !status.Running {
					t.Fatalf("status after the repair: %+v", status)
				}

				// Whole again: the next checks only read, and the rules are not applied again.
				before := len(host.Calls())
				for i := 0; i < 4; i++ {
					checkNow(a)
					a.cycle(ctx, true)
				}
				onlyReads(t, host, before)
				if repairs(logs) != 1 {
					t.Fatalf("the agent repaired a whole firewall again:\n%s", logs)
				}
			})
		}
	}
}

// One rule deleted out of a chain, with everything else in place, is damage too.
func TestOneRuleDeletedFromAChainIsRestored(t *testing.T) {
	a, host, logs := firewallAgent(t)
	if !host.Delete("filter", chainOut, "-j ACCEPT") {
		t.Fatal("the pretend host has no such rule")
	}
	checkNow(a)
	a.cycle(context.Background(), true)
	wholeFirewall(t, a, host)
	if repairs(logs) != 1 || !strings.Contains(logs.String(), "holds 9 of 10 rules") {
		t.Fatalf("log:\n%s", logs)
	}
}

// More rules than the agent put in a chain are not damage: someone else added one (an administrator
// blocking an address in a hurry), or the host's iptables writes one rule as several. Repairing
// that at every check would take the rule away every 30 seconds, and fight with whatever puts it
// back; the next full apply (new settings) replaces the chain anyway. Nothing is applied however
// often it is checked.
func TestExtraRulesInAChainAreNotRepaired(t *testing.T) {
	a, host, logs := firewallAgent(t)
	host.AddRule("filter", chainOut, "-d 192.0.2.77/32 -j DROP")
	before := len(host.Calls())
	for i := 0; i < 5; i++ {
		checkNow(a)
		a.cycle(context.Background(), true)
	}
	onlyReads(t, host, before)
	if repairs(logs) != 0 || !slices.Contains(host.Rules("filter", chainOut), "-d 192.0.2.77/32 -j DROP") {
		t.Fatalf("a rule in the agent's chain made it apply its rules again:\n%s", logs)
	}
	if status := statusOf(t, a); status.Error != "" {
		t.Fatalf("status: %q", status.Error)
	}
}

// What a failed apply left on the host is not known, so nothing is held against the host until an
// apply goes through; and a damaged firewall that cannot be repaired is reported, not counted.
func TestNoCountsBeforeAnApplyWentThrough(t *testing.T) {
	host := hostfwtest.New()
	f := &Firewall{Backend: "nft", Exec: host.Exec}
	d := fixture(t)
	if err := f.Apply(context.Background(), d, nil); err != nil {
		t.Fatal(err)
	}
	if f.rendered["filter/"+chainOut] != 10 {
		t.Fatalf("rules rendered: %v", f.rendered)
	}
	host.FlushChain("filter", chainOut)
	host.Fail(func(name string, args []string) error {
		if strings.HasSuffix(name, "-restore") {
			return errors.New("xtables lock is held by another program")
		}
		return nil
	})
	if err := f.Apply(context.Background(), d, nil); err == nil {
		t.Fatal("the apply did not fail")
	}
	if f.rendered != nil {
		t.Fatalf("a failed apply left counts: %v", f.rendered)
	}
	host.Fail(nil)
	if got := f.Missing(context.Background()); len(got) != 0 {
		t.Fatalf("with no counts only jumps and chains are checked: %v", got)
	}
	if err := f.Apply(context.Background(), d, nil); err != nil {
		t.Fatal(err)
	}
	f.Remove(context.Background())
	if f.rendered != nil {
		t.Fatal("counts survive the removal of the chains")
	}
}

// limitedAgent is firewallAgent with a speed limit on alice's identity, and sessions that the test
// changes; the rules applied are those of no session yet.
func limitedAgent(t *testing.T) (a *Agent, host *hostfwtest.Host, logs *bytes.Buffer, connected *[]ipsecproto.SA) {
	t.Helper()
	host = hostfwtest.New()
	logs = &bytes.Buffer{}
	connected = &[]ipsecproto.SA{}
	a = &Agent{Dir: t.TempDir(), Run: t.TempDir(), Log: log.New(logs, "", 0), FW: &Firewall{Backend: "nft", Exec: host.Exec},
		list: func(context.Context) ([]ipsecproto.SA, error) { return slices.Clone(*connected), nil }}
	d := fixture(t)
	d.SpeedLimits = []ipsecproto.SpeedLimit{{Name: "alice", Identities: []string{"alice-2@ghostlink", "alice-3@ghostlink"}, BytesPerSecond: 500_000}}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := a.FW.Apply(context.Background(), d, nil); err != nil {
		t.Fatal(err)
	}
	a.desired, a.applied, a.lastJumps = d, d.Generation, time.Now()
	return a, host, logs, connected
}

func session(identity, ip string) ipsecproto.SA {
	return ipsecproto.SA{Name: "gl-ikev2-eap", UniqueID: ip, Identity: identity, VirtualIP: ip}
}

// The speed limit rules are the one chain whose rules change while the agent runs: the agent applies
// its whole rule set again whenever a capped client connects or disconnects (every 5 seconds at
// most), and that is a new number of rules, not a loss. A check, even in the same cycle as the
// change, never takes it for damage, whatever the order sessions come and go in.
func TestSessionChangesAreNotMistakenForDamage(t *testing.T) {
	ctx := context.Background()
	a, host, logs, connected := limitedAgent(t)
	limitRulesOnHost := func() int { return len(host.Rules("filter", chainLimit)) }
	restores := func() int { return host.Count("-restore") }

	// Each step is a set of sessions, and the rules the limit chain holds once the cycle has run.
	for i, step := range []struct {
		sessions []ipsecproto.SA
		rules    int
	}{
		{nil, 0},
		{[]ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2")}, 2},
		{[]ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2")}, 2},
		{[]ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2"), session("alice-3@ghostlink", "10.80.0.2")}, 4},
		{[]ipsecproto.SA{session("bob-3@ghostlink", "10.80.0.3")}, 0}, // not capped
		{[]ipsecproto.SA{session("alice-3@ghostlink", "10.80.0.2")}, 2},
		{nil, 0},
	} {
		*connected = step.sessions
		before := restores()
		checkNow(a) // the check comes first in the cycle, with the rules of the sessions before this step
		a.cycle(ctx, true)
		if got := limitRulesOnHost(); got != step.rules {
			t.Fatalf("step %d: the limit chain holds %d rules, want %d", i, got, step.rules)
		}
		if repairs(logs) != 0 {
			t.Fatalf("step %d: a change of sessions was taken for damage:\n%s", i, logs)
		}
		// A step that changes the limit rules is applied once (three tables); the others not at all.
		if applied := restores() - before; applied != 0 && applied != 3 {
			t.Fatalf("step %d: %d restores", i, applied)
		}
	}

	// And with the sessions steady, the checks only read.
	*connected = []ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2")}
	checkNow(a)
	a.cycle(ctx, true)
	before := len(host.Calls())
	for i := 0; i < 5; i++ {
		checkNow(a)
		a.cycle(ctx, true)
	}
	onlyReads(t, host, before)
	if status := statusOf(t, a); status.Error != "" {
		t.Fatalf("status: %q", status.Error)
	}
}

// A change of sessions whose rules could not be applied (iptables is busy) leaves the host with the
// rules of the sessions before; the checks in between do not take that for damage and do not
// apply anything of their own, and the next cycle that can applies the limit rules.
func TestFailedLimitApplyIsNotMistakenForDamage(t *testing.T) {
	ctx := context.Background()
	a, host, logs, connected := limitedAgent(t)
	*connected = []ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2")}
	checkNow(a)
	a.cycle(ctx, true)
	if len(host.Rules("filter", chainLimit)) != 2 {
		t.Fatal("the first session got no limit rules")
	}

	host.Fail(func(name string, args []string) error {
		if strings.HasSuffix(name, "-restore") {
			return errors.New("xtables lock is held by another program")
		}
		return nil
	})
	*connected = []ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2"), session("alice-3@ghostlink", "10.80.0.2")}
	for i := 0; i < 3; i++ {
		checkNow(a)
		a.cycle(ctx, true)
	}
	if repairs(logs) != 0 {
		t.Fatalf("a failed apply of the limit rules was taken for a damaged firewall:\n%s", logs)
	}
	if status := statusOf(t, a); !strings.Contains(status.Error, "speed limits not applied") {
		t.Fatalf("the status does not report the limits that are not in force: %q", status.Error)
	}

	host.Fail(nil)
	checkNow(a)
	a.cycle(ctx, true)
	if len(host.Rules("filter", chainLimit)) != 4 || repairs(logs) != 0 {
		t.Fatalf("limit chain %v, repairs %d:\n%s", host.Rules("filter", chainLimit), repairs(logs), logs)
	}
	if status := statusOf(t, a); status.Error != "" {
		t.Fatalf("status: %q", status.Error)
	}
}

// The speed limit chain flushed on its own, while its sessions stay: its rules are not in force,
// the agent believes they are (they are the ones it rendered), and the number of rules is how it
// finds out.
func TestFlushedLimitChainIsRestored(t *testing.T) {
	ctx := context.Background()
	a, host, logs, connected := limitedAgent(t)
	*connected = []ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2")}
	checkNow(a)
	a.cycle(ctx, true)
	want := slices.Clone(host.Rules("filter", chainLimit))
	if len(want) != 2 {
		t.Fatalf("limit rules %v", want)
	}
	host.FlushChain("filter", chainLimit)
	checkNow(a)
	a.cycle(ctx, true)
	if got := host.Rules("filter", chainLimit); !slices.Equal(got, want) {
		t.Fatalf("the limit chain holds %v, want %v", got, want)
	}
	if repairs(logs) != 1 || !strings.Contains(logs.String(), "filter/"+chainLimit+" holds 0 of 2 rules") {
		t.Fatalf("log:\n%s", logs)
	}
	before := len(host.Calls())
	for i := 0; i < 3; i++ {
		checkNow(a)
		a.cycle(ctx, true)
	}
	onlyReads(t, host, before)
}

// The check compares numbers of rules, which is only right while a line the agent renders is one
// rule on the host. A line that expands when iptables-restore reads it makes the host hold more
// than the agent counted, which Missing does not hold against it (see hostfw.CountRules), but it
// would hide the loss of one such rule from the count, so the render keeps to one rule per line:
// no list of addresses, no list of ports. A change here is a reason to look at hostfw.CountRules.
func TestRenderedRulesAreOneRuleEach(t *testing.T) {
	d := fixture(t)
	d.Firewall.Internal = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"}
	d.Firewall.Blocked = []string{"203.0.113.0/24", "198.51.100.0/24"}
	d.Firewall.Ports = []string{"25", "6881-6889", "8080"}
	d.SpeedLimits = []ipsecproto.SpeedLimit{{Name: "alice", Identities: []string{"alice-2@ghostlink", "bob-3@ghostlink"}, BytesPerSecond: 500_000}}
	sas := []ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2"), session("bob-3@ghostlink", "10.80.0.2")}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	// Options whose value is a single address, network, port or range.
	single := []string{"-s", "-d", "-i", "-o", "--sport", "--dport", "--to-destination", "--set-mss", "--hashlimit-above", "--hashlimit-name"}
	rules := 0
	for table, input := range Rules(d, sas) {
		for _, line := range strings.Split(input, "\n") {
			if !strings.HasPrefix(line, "-A ") {
				continue
			}
			rules++
			fields := strings.Fields(line)
			if strings.Contains(line, "multiport") || strings.Contains(line, "iprange") {
				t.Errorf("%s: %q matches a list of ports or addresses", table, line)
			}
			for i, f := range fields {
				if slices.Contains(single, f) && i+1 < len(fields) && strings.Contains(fields[i+1], ",") {
					t.Errorf("%s: %q gives %s a list (%s), which iptables-restore expands into several rules", table, line, f, fields[i+1])
				}
			}
		}
	}
	if rules == 0 {
		t.Fatal("no rules rendered")
	}
}

// The number of rules the fixture gives each chain, worked out by hand from Rules: a change of the
// render that changes a number must change it here, which is the moment to think of the check.
func TestRenderedRuleCounts(t *testing.T) {
	d := fixture(t) // two pools; one IPv4 internal range, one blocked range; two ports, both for tcp and udp; SMTP; a server IP
	got := hostfw.CountRules(Rules(d, nil))
	want := map[string]int{
		"filter/" + chainForward: 1 + 2*2,
		"filter/" + chainOut:     1 + 1 + 1 + 1 + 2*2 + 1 + 1,
		"filter/" + chainInput:   2 * 3,
		"nat/" + chainDNAT:       2 * 2,
		"nat/" + chainNAT:        2,
		"mangle/" + chainMSS:     2 * 2,
	}
	for chain, n := range want {
		if got[chain] != n {
			t.Errorf("%s: %d rules, want %d", chain, got[chain], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("chains with rules: %v, want %v (the limit chain is empty without sessions)", got, want)
	}
	d.SpeedLimits = []ipsecproto.SpeedLimit{{Name: "alice", Identities: []string{"alice-2@ghostlink"}, BytesPerSecond: 500_000}}
	sas := []ipsecproto.SA{session("alice-2@ghostlink", "10.79.0.2"), session("alice-2@ghostlink", "10.79.0.2"), session("bob-3@ghostlink", "10.80.0.2")}
	if n := hostfw.CountRules(Rules(d, sas))["filter/"+chainLimit]; n != 2 {
		t.Errorf("two sessions on one address and one uncapped: %d limit rules, want 2 (one per direction)", n)
	}
}
