// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package hostfw

import (
	"context"
	"slices"
	"strings"
	"testing"

	"ghostlink.local/outline-worker/internal/hostfw/hostfwtest"
)

// The count of a chain's rules is the number of lines "-A CHAIN ..." in what is restored: the
// declarations of the chains, the table lines, comments and COMMIT are not rules, and a chain whose
// name starts like another's does not count for it.
func TestCountRules(t *testing.T) {
	got := CountRules(map[string]string{
		"filter": "# a comment\n*filter\n:T-IN - [0:0]\n:T-INX - [0:0]\n:T-EMPTY - [0:0]\n\n" +
			"-A T-IN -s 192.0.2.0/24 -j DROP\n-A T-IN -j ACCEPT\n  -A T-IN -p tcp -j DROP  \n-A T-INX -j DROP\n-A T-BARE\nCOMMIT\n",
		"nat": "*nat\n:T-POST - [0:0]\n-A T-POST -s 192.0.2.0/24 -j MASQUERADE\nCOMMIT\n",
	})
	want := map[string]int{"filter/T-IN": 3, "filter/T-INX": 1, "filter/T-BARE": 1, "nat/T-POST": 1}
	if len(got) != len(want) {
		t.Fatalf("counts %v, want %v", got, want)
	}
	for chain, n := range want {
		if got[chain] != n {
			t.Errorf("%s: %d rules, want %d", chain, got[chain], n)
		}
	}
	if len(CountRules(nil)) != 0 || len(CountRules(map[string]string{"filter": ""})) != 0 {
		t.Error("an empty render has rules")
	}
	// `iptables -S` writes the same rules back as "-A CHAIN ...", after "-N CHAIN" (or "-P" for a built-in chain).
	if n := countIn("-N T-IN\n-A T-IN -j DROP\n-A T-INX -j DROP\n-A T-IN\n", "T-IN"); n != 2 {
		t.Errorf("countIn: %d, want 2", n)
	}
}

// threeRules is an agent with one chain of three rules, jumped to from FORWARD.
var (
	threeJumps = []Jump{{Table: "filter", Chain: "FORWARD", Target: "T-OUT"}}
	threeChain = map[string][]string{"filter": {"T-OUT"}}
	threeRules = map[string]string{
		"filter": "*filter\n:T-OUT - [0:0]\n-A T-OUT -d 192.0.2.0/24 -j DROP\n-A T-OUT -d 198.51.100.0/24 -j REJECT\n-A T-OUT -j ACCEPT\nCOMMIT\n",
	}
)

func appliedThreeRules(t *testing.T) (*Firewall, *hostfwtest.Host, map[string]int) {
	t.Helper()
	fw, host := fakeFirewall()
	ctx := context.Background()
	if err := fw.Restore(ctx, threeRules); err != nil {
		t.Fatal(err)
	}
	if err := fw.EnsureJumps(ctx, threeJumps); err != nil {
		t.Fatal(err)
	}
	want := CountRules(threeRules)
	if want["filter/T-OUT"] != 3 {
		t.Fatalf("counted %v", want)
	}
	if got := fw.Missing(ctx, threeJumps, threeChain, want); len(got) != 0 {
		t.Fatalf("a restored firewall is missing %v", got)
	}
	return fw, host, want
}

// damage does to the pretend host what something else on a real host may do to the chain.
func damage(host *hostfwtest.Host, kind string) {
	switch kind {
	case "flush":
		host.FlushChain("filter", "T-OUT")
	case "delete":
		host.Delete("filter", "T-OUT", "-j ACCEPT")
	case "extra":
		host.AddRule("filter", "T-OUT", "-s 203.0.113.9/32 -j DROP")
	case "rewrite":
		// What the host's iptables may print differently: the same rules, other text.
		host.FlushChain("filter", "T-OUT")
		for _, rule := range []string{"-d 192.0.2.0/24 -j DROP", "-d 198.51.100.0/24 -j REJECT --reject-with icmp-port-unreachable", "-j ACCEPT"} {
			host.AddRule("filter", "T-OUT", rule)
		}
	}
}

// iptables -F on one chain leaves its jump in place and the chain existing, so the old check (the
// jumps and the chains) sees a whole firewall; the number of rules does not.
func TestMissingNoticesAChainFlushedWhileItsJumpStays(t *testing.T) {
	ctx := context.Background()
	fw, host, want := appliedThreeRules(t)
	damage(host, "flush")
	if !fw.HasJumps(ctx, threeJumps) {
		t.Fatal("the flush took the jump away")
	}
	if got := fw.Missing(ctx, threeJumps, threeChain, nil); len(got) != 0 {
		t.Fatalf("without counts a flushed chain with its jump looks whole: %v (this is the old behaviour)", got)
	}
	if got, wanted := fw.Missing(ctx, threeJumps, threeChain, want), []string{"chain filter/T-OUT holds 0 of 3 rules"}; !slices.Equal(got, wanted) {
		t.Fatalf("flushed chain: %v, want %v", got, wanted)
	}
	// Restoring the rules puts the chain right.
	if err := fw.Restore(ctx, threeRules); err != nil {
		t.Fatal(err)
	}
	if got := fw.Missing(ctx, threeJumps, threeChain, want); len(got) != 0 {
		t.Fatalf("after restoring: %v", got)
	}
}

// One rule deleted out of a chain is damage too.
func TestMissingNoticesARuleDeleted(t *testing.T) {
	fw, host, want := appliedThreeRules(t)
	damage(host, "delete")
	if got, wanted := fw.Missing(context.Background(), threeJumps, threeChain, want), []string{"chain filter/T-OUT holds 2 of 3 rules"}; !slices.Equal(got, wanted) {
		t.Fatalf("one rule deleted: %v, want %v", got, wanted)
	}
}

// A chain that is gone is reported as gone, not as short of rules.
func TestMissingReportsAGoneChainOnce(t *testing.T) {
	fw, _ := fakeFirewall()
	got := fw.Missing(context.Background(), nil, threeChain, map[string]int{"filter/T-OUT": 3})
	if !slices.Equal(got, []string{"chain filter/T-OUT"}) {
		t.Fatalf("a chain that does not exist: %v", got)
	}
}

// What is not damage is not repaired, however often it is checked (a repair every 30 seconds that
// has nothing to repair would restart the speed limits and fill the log): the same number of rules
// in other words, more rules than were put there (someone else's, or a rule the host's iptables
// splits), a chain nobody gave a count for.
func TestMissingDoesNotFlap(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, change string
		want         func(map[string]int) map[string]int
	}{
		{"nothing changed", "", func(w map[string]int) map[string]int { return w }},
		{"the rules written back in other words", "rewrite", func(w map[string]int) map[string]int { return w }},
		{"a rule added by someone else", "extra", func(w map[string]int) map[string]int { return w }},
		{"no count for the chain", "flush", func(map[string]int) map[string]int { return map[string]int{"nat/OTHER": 4} }},
		{"no counts at all", "flush", func(map[string]int) map[string]int { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			fw, host, want := appliedThreeRules(t)
			damage(host, c.change)
			want = c.want(want)
			before := len(host.Calls())
			for i := 0; i < 5; i++ {
				if got := fw.Missing(ctx, threeJumps, threeChain, want); len(got) != 0 {
					t.Fatalf("check %d: %v", i+1, got)
				}
			}
			for _, call := range host.Calls()[before:] {
				if !strings.Contains(call, " -C ") && !strings.Contains(call, " -S ") {
					t.Fatalf("a check ran %q", call)
				}
			}
		})
	}
}

// Counting costs no command of its own: the chain's rules come from the -S that asks whether the
// chain exists.
func TestMissingCountsInTheSameCommandThatFindsTheChain(t *testing.T) {
	fw, host, want := appliedThreeRules(t)
	before := len(host.Calls())
	fw.Missing(context.Background(), threeJumps, threeChain, want)
	if calls := host.Calls()[before:]; len(calls) != 2 || !strings.Contains(calls[0], " -C ") || !strings.Contains(calls[1], " -S T-OUT") {
		t.Fatalf("a check of one jump and one chain ran %v", calls)
	}
}
