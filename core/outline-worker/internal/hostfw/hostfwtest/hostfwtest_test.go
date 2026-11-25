// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package hostfwtest

import (
	"context"
	"slices"
	"testing"
)

func run(t *testing.T, h *Host, wantOK bool, stdin, name string, args ...string) string {
	t.Helper()
	out, err := h.Exec(context.Background(), stdin, name, args...)
	if (err == nil) != wantOK {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return out
}

// The fake must behave like iptables where the agents rely on it, or a test that passes against it
// proves nothing.
func TestHostBehavesLikeIptablesWhereAgentsRelyOnIt(t *testing.T) {
	h := New()
	// A jump to a chain that is not there is refused; once the chain exists it is inserted first.
	run(t, h, false, "", "iptables-nft", "-w", "5", "-t", "filter", "-I", "FORWARD", "1", "-j", "X-FWD")
	run(t, h, true, "", "iptables-nft", "-w", "5", "-t", "filter", "-N", "X-FWD")
	run(t, h, true, "", "iptables-nft", "-w", "5", "-t", "filter", "-A", "FORWARD", "-j", "ACCEPT")
	run(t, h, true, "", "iptables-nft", "-w", "5", "-t", "filter", "-I", "FORWARD", "1", "-i", "x0", "-j", "X-FWD")
	if got := h.Rules("filter", "FORWARD"); !slices.Equal(got, []string{"-i x0 -j X-FWD", "-j ACCEPT"}) {
		t.Fatalf("FORWARD: %v", got)
	}
	run(t, h, true, "", "iptables-nft", "-t", "filter", "-C", "FORWARD", "-i", "x0", "-j", "X-FWD")
	run(t, h, false, "", "iptables-nft", "-t", "filter", "-C", "FORWARD", "-i", "x1", "-j", "X-FWD")
	// A chain that is jumped to, or holds rules, cannot be deleted; a built-in chain never.
	run(t, h, false, "", "iptables-nft", "-t", "filter", "-X", "X-FWD")
	run(t, h, false, "", "iptables-nft", "-t", "filter", "-X", "FORWARD")
	run(t, h, true, "", "iptables-nft", "-t", "filter", "-D", "FORWARD", "-i", "x0", "-j", "X-FWD")
	run(t, h, true, "", "iptables-nft", "-t", "filter", "-X", "X-FWD")
	if out := run(t, h, true, "", "iptables-nft", "-t", "filter", "-S", "FORWARD"); out != "-P FORWARD ACCEPT\n-A FORWARD -j ACCEPT\n" {
		t.Fatalf("-S: %q", out)
	}
	// An unknown backend has no command.
	run(t, h, false, "", "iptables-", "-t", "filter", "-S", "FORWARD")
}

// iptables-restore --noflush empties and rebuilds the chains it declares, leaves the others, and
// changes a table all at once or not at all.
func TestRestoreReplacesDeclaredChainsAtomically(t *testing.T) {
	h := New()
	h.AddChain("filter", "OTHER")
	run(t, h, true, "", "iptables-nft", "-t", "filter", "-A", "OTHER", "-j", "DROP")
	good := "*filter\n:X-A - [0:0]\n:X-B - [0:0]\n-A X-A -j X-B\n-A X-B -j ACCEPT\nCOMMIT\n"
	run(t, h, true, good, "iptables-nft-restore", "--noflush", "-w", "5")
	if !slices.Equal(h.Rules("filter", "X-A"), []string{"-j X-B"}) || !slices.Equal(h.Rules("filter", "OTHER"), []string{"-j DROP"}) {
		t.Fatalf("restored: %v %v", h.Rules("filter", "X-A"), h.Rules("filter", "OTHER"))
	}
	// Restoring again replaces the declared chains instead of adding to them.
	run(t, h, true, good, "iptables-nft-restore", "--noflush", "-w", "5")
	if len(h.Rules("filter", "X-B")) != 1 {
		t.Fatalf("restoring twice: %v", h.Rules("filter", "X-B"))
	}
	// A rule that fails (a jump to a chain nobody declared) leaves the table as it was.
	bad := "*filter\n:X-A - [0:0]\n-A X-A -j DROP\n-A X-A -j NOWHERE\nCOMMIT\n"
	run(t, h, false, bad, "iptables-nft-restore", "--noflush", "-w", "5")
	if !slices.Equal(h.Rules("filter", "X-A"), []string{"-j X-B"}) {
		t.Fatalf("a failed restore changed the table: %v", h.Rules("filter", "X-A"))
	}
	// Flush keeps the chains, Reset takes the ones that are not built in.
	h.Flush()
	if !h.Has("filter", "X-A") || len(h.Rules("filter", "X-A")) != 0 || len(h.Rules("filter", "OTHER")) != 0 {
		t.Fatal("Flush")
	}
	h.Reset()
	if h.Has("filter", "X-A") || h.Has("filter", "OTHER") || !h.Has("filter", "FORWARD") || len(h.Chains()) != 0 {
		t.Fatalf("Reset left %v", h.Chains())
	}
}
