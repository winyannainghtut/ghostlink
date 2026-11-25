// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package hostfw

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"ghostlink.local/outline-worker/internal/hostfw/hostfwtest"
)

// An agent on the host follows the backend the host's iptables command uses, which Docker uses
// too, so it is right even at boot before anyone added rules.
func TestBackendOf(t *testing.T) {
	for version, want := range map[string]string{
		"iptables v1.8.7 (nf_tables)\n": "nft",
		"iptables v1.8.4 (legacy)\n":    "legacy",
		"iptables v1.6.1\n":             "",
		"":                              "",
	} {
		if got := backendOf(version); got != want {
			t.Errorf("%q: %q, want %q", version, got, want)
		}
	}
}

// Only the filter FORWARD jumps marked Docker get a copy in DOCKER-USER: that chain sees forwarded
// traffic only, ahead of Docker's accept rules. The others (the IPsec component's, whose chain
// counts speed limits) are left alone.
func TestDockerCopies(t *testing.T) {
	jumps := []Jump{
		{Table: "filter", Chain: "INPUT", Match: []string{"-i", "glmesh0"}, Target: "IN"},
		{Table: "filter", Chain: "FORWARD", Match: []string{"-i", "glmesh0"}, Target: "FWD", Docker: true},
		{Table: "filter", Chain: "FORWARD", Match: []string{"-o", "glmesh0"}, Target: "OUT", Docker: true},
		{Table: "filter", Chain: "FORWARD", Target: "OTHER"},
		{Table: "mangle", Chain: "FORWARD", Target: "MSS"},
	}
	copies := dockerCopies(jumps)
	if len(copies) != 2 || copies[0].Chain != dockerChain || copies[0].Target != "FWD" || copies[1].Target != "OUT" ||
		strings.Join(copies[1].args("-I", "1"), " ") != "-t filter -I DOCKER-USER 1 -o glmesh0 -j OUT" {
		t.Fatalf("copies: %+v", copies)
	}
	if jumps[1].Chain != "FORWARD" {
		t.Fatal("dockerCopies changed the jumps it was given")
	}
}

// testRules is a small agent: three chains in two tables, and a jump to each from a built-in chain.
var (
	testJumps = []Jump{
		{Table: "filter", Chain: "FORWARD", Target: "T-FWD"},
		{Table: "filter", Chain: "INPUT", Match: []string{"-i", "gltest0"}, Target: "T-IN"},
		{Table: "nat", Chain: "POSTROUTING", Target: "T-POST"},
	}
	testChains = map[string][]string{"filter": {"T-FWD", "T-IN"}, "nat": {"T-POST"}}
	testRender = map[string]string{
		"filter": "*filter\n:T-FWD - [0:0]\n:T-IN - [0:0]\n-A T-FWD -j T-IN\n-A T-IN -j DROP\nCOMMIT\n",
		"nat":    "*nat\n:T-POST - [0:0]\n-A T-POST -s 192.0.2.0/24 -j MASQUERADE\nCOMMIT\n",
	}
)

func fakeFirewall() (*Firewall, *hostfwtest.Host) {
	host := hostfwtest.New()
	return &Firewall{Backend: "nft", Exec: host.Exec}, host
}

// A firewall reload takes the agent's jumps away, and with them often its chains. Missing says
// what is gone, so the agent can restore all of it: a jump put back into a chain that was flushed,
// or never made again, protects nothing.
func TestMissingFindsWhatAReloadTakesAway(t *testing.T) {
	ctx := context.Background()
	fw, host := fakeFirewall()
	if got := fw.Missing(ctx, testJumps, testChains); len(got) != 6 {
		t.Fatalf("an empty host is missing 3 jumps and 3 chains, Missing says %v", got)
	}
	if err := fw.Restore(ctx, testRender); err != nil {
		t.Fatal(err)
	}
	if err := fw.EnsureJumps(ctx, testJumps); err != nil {
		t.Fatal(err)
	}
	if got := fw.Missing(ctx, testJumps, testChains); len(got) != 0 || !fw.HasJumps(ctx, testJumps) {
		t.Fatalf("a restored firewall is missing %v", got)
	}

	// iptables -F: the rules of every chain go, the jumps among them; the chains stay, empty.
	host.Flush()
	got := fw.Missing(ctx, testJumps, testChains)
	want := []string{"jump filter/FORWARD -> T-FWD", "jump filter/INPUT -> T-IN", "jump nat/POSTROUTING -> T-POST"}
	if !slices.Equal(got, want) || fw.HasJumps(ctx, testJumps) {
		t.Fatalf("after iptables -F: %v, want %v", got, want)
	}
	// Putting the jumps back alone (what EnsureJumps does) leaves the chains empty: they still
	// jump, and filter nothing. That is why Missing is not enough to be "fixed" by EnsureJumps.
	if err := fw.EnsureJumps(ctx, testJumps); err != nil {
		t.Fatal(err)
	}
	if got := fw.Missing(ctx, testJumps, testChains); len(got) != 0 {
		t.Fatalf("with its jumps back the firewall looks whole: %v", got)
	}
	if len(host.Rules("filter", "T-IN")) != 0 {
		t.Fatal("EnsureJumps restored rules")
	}

	// A reload that replaces everything (netfilter-persistent, iptables-restore): the chains go too,
	// and a jump cannot be put back until they exist.
	host.Reset()
	if err := fw.EnsureJumps(ctx, testJumps); err == nil {
		t.Fatal("a jump to a chain that does not exist was inserted")
	}
	if got := fw.Missing(ctx, testJumps, testChains); len(got) != 6 {
		t.Fatalf("after a full reload: %v", got)
	}
	if err := fw.Restore(ctx, testRender); err != nil {
		t.Fatal(err)
	}
	if err := fw.EnsureJumps(ctx, testJumps); err != nil {
		t.Fatal(err)
	}
	if got := fw.Missing(ctx, testJumps, testChains); len(got) != 0 || len(host.Rules("filter", "T-IN")) != 1 {
		t.Fatalf("restored after a full reload: missing %v, rules %v", got, host.Rules("filter", "T-IN"))
	}

	// One jump gone, the rest in place.
	host.Delete("nat", "POSTROUTING", "-j T-POST")
	if got := fw.Missing(ctx, testJumps, testChains); !slices.Equal(got, []string{"jump nat/POSTROUTING -> T-POST"}) {
		t.Fatalf("one jump gone: %v", got)
	}
}

// Checking an intact firewall costs one command per jump and per chain, and changes nothing.
func TestMissingChangesNothingAndAsksDockerOnlyWhenNeeded(t *testing.T) {
	ctx := context.Background()
	fw, host := fakeFirewall()
	if err := fw.Restore(ctx, testRender); err != nil {
		t.Fatal(err)
	}
	if err := fw.EnsureJumps(ctx, testJumps); err != nil {
		t.Fatal(err)
	}
	before := len(host.Calls())
	if got := fw.Missing(ctx, testJumps, testChains); len(got) != 0 {
		t.Fatal(got)
	}
	calls := host.Calls()[before:]
	if len(calls) != 6 || host.Count(" -S DOCKER-USER") != 0 {
		t.Fatalf("checking an intact firewall ran %d commands (want 3 jumps + 3 chains, none about Docker): %v", len(calls), calls)
	}
	for _, call := range calls {
		if !strings.Contains(call, " -C ") && !strings.Contains(call, " -S ") {
			t.Fatalf("a check changed the host: %s", call)
		}
	}
	// A jump marked Docker is also kept in DOCKER-USER once Docker made it, so then the host is asked.
	docker := append(slices.Clone(testJumps), Jump{Table: "filter", Chain: "FORWARD", Match: []string{"-i", "gltest0"}, Target: "T-FWD", Docker: true})
	if got := fw.Missing(ctx, docker, testChains); !slices.Equal(got, []string{"jump filter/FORWARD -> T-FWD"}) || host.Count(" -S DOCKER-USER") != 1 {
		t.Fatalf("without DOCKER-USER: %v, %d questions about it", got, host.Count(" -S DOCKER-USER"))
	}
	host.AddChain("filter", "DOCKER-USER")
	got := fw.Missing(ctx, docker, testChains)
	if !slices.Equal(got, []string{"jump filter/FORWARD -> T-FWD", "jump filter/DOCKER-USER -> T-FWD"}) {
		t.Fatalf("with DOCKER-USER: %v", got)
	}
	if err := fw.EnsureJumps(ctx, docker); err != nil {
		t.Fatal(err)
	}
	if got := fw.Missing(ctx, docker, testChains); len(got) != 0 {
		t.Fatalf("after EnsureJumps: %v", got)
	}
}

// Every command goes through Exec when it is set, and a failing command is the caller's error.
func TestExecIsUsedForEveryCommand(t *testing.T) {
	ctx := context.Background()
	fw, host := fakeFirewall()
	boom := errors.New("xtables lock")
	host.Fail(func(name string, args []string) error {
		if strings.HasSuffix(name, "-restore") {
			return boom
		}
		return nil
	})
	if err := fw.Restore(ctx, testRender); !errors.Is(err, boom) {
		t.Fatalf("Restore: %v", err)
	}
	host.Fail(nil)
	if err := fw.Restore(ctx, testRender); err != nil {
		t.Fatal(err)
	}
	if err := fw.EnsureJumps(ctx, testJumps); err != nil {
		t.Fatal(err)
	}
	fw.Remove(ctx, testJumps, testChains)
	if left := host.Chains(); len(left) != 0 {
		t.Fatalf("Remove left %v", left)
	}
	for _, table := range []string{"filter", "nat"} {
		for _, chain := range []string{"FORWARD", "INPUT", "POSTROUTING"} {
			if rules := host.Rules(table, chain); len(rules) != 0 {
				t.Fatalf("Remove left %s/%s: %v", table, chain, rules)
			}
		}
	}
	// An unknown backend still refuses to restore, without running anything.
	before := len(host.Calls())
	if err := (&Firewall{Exec: host.Exec}).Restore(ctx, testRender); err == nil || len(host.Calls()) != before {
		t.Fatalf("a firewall with no backend restored: %v", err)
	}
}
