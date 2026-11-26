// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

// Package hostfw manages a Ghostlink agent's own iptables chains on a host. An agent renders its
// chains as iptables-restore input; Restore replaces exactly those chains (atomically per table,
// with --noflush), EnsureJumps keeps one jump to each from its built-in chain (and from Docker's
// DOCKER-USER for forwarded traffic), Missing and HasJumps tell whether something on the host (a
// firewall reload, iptables -F) took them away, and Remove takes everything away again. Rules
// from other software are never touched. It uses the iptables backend the host already uses (nft
// or legacy), because a packet must pass both backends.
package hostfw

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Jump is a rule in a built-in chain that sends matching packets to an agent chain.
type Jump struct {
	Table  string
	Chain  string
	Match  []string
	Target string
	// Docker: a filter FORWARD jump also kept first in Docker's DOCKER-USER (see dockerChain).
	// Only for a chain that ends every packet it sees with ACCEPT or DROP, or that does nothing
	// but accept, since a packet can pass it twice: once from DOCKER-USER, once from FORWARD.
	Docker bool
}

func (j Jump) args(op string, position ...string) []string {
	args := append([]string{"-t", j.Table, op, j.Chain}, position...)
	args = append(args, j.Match...)
	return append(args, "-j", j.Target)
}

// Exec runs one command: its name, its arguments and what it reads from standard input. Run is
// the real one.
type Exec func(ctx context.Context, stdin, name string, args ...string) (string, error)

// Firewall applies rules with one backend.
type Firewall struct {
	Backend string // "nft" or "legacy"
	// Exec runs the iptables commands. nil means Run; a test gives it a fake host (see
	// hostfwtest), so what an agent does after a firewall reload can be tried without iptables.
	Exec Exec
}

func (f *Firewall) run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if f.Exec != nil {
		return f.Exec(ctx, stdin, name, args...)
	}
	return Run(ctx, stdin, name, args...)
}

// Run executes a command with a timeout, feeding it stdin.
func Run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Detect picks the backend with more rules in it (as Kubernetes' iptables wrapper does); a fresh
// host with neither picks nft.
func Detect(ctx context.Context) string {
	count := func(backend string) int {
		out, err := Run(ctx, "", "iptables-"+backend+"-save")
		if err != nil {
			return -1
		}
		return strings.Count(out, "\n-A ")
	}
	if count("legacy") > count("nft") {
		return "legacy"
	}
	return "nft"
}

// DetectHost is Detect for an agent running on the host itself (not in a container): it uses
// the backend the host's iptables command points to, as Docker, ufw and firewalld do. That is
// known even at boot, before any of them added rules to count.
func DetectHost(ctx context.Context) string {
	out, _ := Run(ctx, "", "iptables", "-V")
	if backend := backendOf(out); backend != "" {
		return backend
	}
	return Detect(ctx)
}

// backendOf reads the backend from `iptables -V` ("iptables v1.8.7 (nf_tables)"); versions
// before 1.8 do not say.
func backendOf(version string) string {
	switch {
	case strings.Contains(version, "(nf_tables)"):
		return "nft"
	case strings.Contains(version, "(legacy)"):
		return "legacy"
	}
	return ""
}

func (f *Firewall) iptables(ctx context.Context, args ...string) error {
	_, err := f.iptablesOut(ctx, args...)
	return err
}

// iptablesOut is iptables for a command whose output is wanted.
func (f *Firewall) iptablesOut(ctx context.Context, args ...string) (string, error) {
	return f.run(ctx, "", "iptables-"+f.Backend, append([]string{"-w", "5"}, args...)...)
}

// tableNames are the tables an agent's chains are in, in the order they are restored.
var tableNames = []string{"filter", "nat", "mangle"}

// Restore replaces the chains declared in each table's iptables-restore input.
func (f *Firewall) Restore(ctx context.Context, tables map[string]string) error {
	if f.Backend != "nft" && f.Backend != "legacy" {
		return errors.New("unknown iptables backend")
	}
	for _, table := range tableNames {
		if tables[table] == "" {
			continue
		}
		if _, err := f.run(ctx, tables[table], "iptables-"+f.Backend+"-restore", "--noflush", "-w", "5"); err != nil {
			return err
		}
	}
	return nil
}

// dockerChain is where Docker lets other software filter forwarded traffic. Docker keeps its
// jump to DOCKER-USER first in FORWARD, ahead of the rules that accept traffic to its containers'
// published ports, and never changes what is in it. A jump that is first in FORWARD does not
// stay first: Docker puts its own rules ahead of it when it starts later. So a filter FORWARD
// jump marked Docker is also kept first in DOCKER-USER while that chain exists, and the agent's
// rules run before Docker's accept rules, without competing with Docker for the first place in
// FORWARD (code review 2026-11-09, F1).
const dockerChain = "DOCKER-USER"

// withDocker adds the DOCKER-USER copies of the jumps marked Docker, when the host has that chain.
// With no jump marked Docker there is nothing to copy, so the host is not asked about the chain.
func (f *Firewall) withDocker(ctx context.Context, jumps []Jump) []Jump {
	copies := dockerCopies(jumps)
	if len(copies) == 0 || f.iptables(ctx, "-t", "filter", "-S", dockerChain) != nil {
		return jumps
	}
	return append(slices.Clone(jumps), copies...)
}

// dockerCopies are the DOCKER-USER copies of the filter FORWARD jumps marked Docker.
func dockerCopies(jumps []Jump) []Jump {
	var copies []Jump
	for _, j := range jumps {
		if j.Docker && j.Table == "filter" && j.Chain == "FORWARD" {
			j.Chain = dockerChain
			copies = append(copies, j)
		}
	}
	return copies
}

// EnsureJumps puts back a jump someone removed (for example a firewall reload), first in its
// chain, and adds the DOCKER-USER copies once Docker made that chain. It does not touch the rules
// in the agent's chains, which a reload that removed the jump has usually emptied or deleted too
// (see Missing): the rules come back with Restore.
func (f *Firewall) EnsureJumps(ctx context.Context, jumps []Jump) error {
	for _, j := range f.withDocker(ctx, jumps) {
		if f.iptables(ctx, j.args("-C")...) == nil {
			continue
		}
		if err := f.iptables(ctx, j.args("-I", "1")...); err != nil {
			return err
		}
	}
	return nil
}

// HasJumps reports whether every jump is in place, the DOCKER-USER copies included.
func (f *Firewall) HasJumps(ctx context.Context, jumps []Jump) bool {
	for _, j := range f.withDocker(ctx, jumps) {
		if f.iptables(ctx, j.args("-C")...) != nil {
			return false
		}
	}
	return true
}

// ruleChain returns the chain a line of iptables-restore input or of `iptables -S` output appends
// a rule to ("-A CHAIN ..."), and false for any other line (a declaration, a policy, COMMIT).
func ruleChain(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "-A ")
	if !ok {
		return "", false
	}
	chain, _, _ := strings.Cut(rest, " ")
	return chain, chain != ""
}

// CountRules counts the rules each chain gets from iptables-restore input (the argument of
// Restore): the lines "-A CHAIN ...", by "table/CHAIN". A chain that is declared and gets no rule
// counts 0, or is not in the result. The agent passes what it restored to Missing, so the check
// compares against what was applied, however the host's iptables writes the rules back.
//
// One line is one rule only while it does not expand when iptables-restore reads it: a list of
// addresses ("-s a,b", "-d a,b") becomes one rule for each address (iptables' manual says so), so
// the host then holds more rules than the input has lines. Missing treats more as nothing lost, so
// a count that is too low never causes a repair; an agent whose render uses such lists loses only
// the notice of one rule deleted out of a chain, not that of a flushed chain. (Port ranges
// "--dport 6881:6889" and the multiport match are one rule.) The IPsec agent's render has no list;
// a test there keeps it so.
func CountRules(tables map[string]string) map[string]int {
	counts := map[string]int{}
	for table, input := range tables {
		for _, line := range strings.Split(input, "\n") {
			if chain, ok := ruleChain(line); ok {
				counts[table+"/"+chain]++
			}
		}
	}
	return counts
}

// countIn counts the rules in the output of `iptables -S CHAIN`.
func countIn(output, chain string) int {
	n := 0
	for _, line := range strings.Split(output, "\n") {
		if name, ok := ruleChain(line); ok && name == chain {
			n++
		}
	}
	return n
}

// Missing lists what the host has lost of an agent's firewall: jumps that are not in place (the
// DOCKER-USER copies included), chains that do not exist (table -> chains), and chains that hold
// fewer rules than the agent put in them. A firewall reload, netfilter-persistent or iptables -F
// takes the jumps away and, with them, usually the rules in the chains, or the chains themselves;
// iptables -F CHAIN empties one chain and leaves its jump. A jump put back into a chain that is
// empty protects nothing, so an agent that finds something missing must restore the whole rule set
// (Restore, then EnsureJumps), not only the jumps. Nothing in the list means nothing is missing.
//
// want is how many rules each chain should hold, "table/CHAIN" -> number, from CountRules of what
// the agent restored last; nil skips the counts (and so does a chain it does not name). Only
// the number is compared, never the text of the rules: iptables writes a rule back differently
// from how it was given, in a way that depends on its version and backend, and a comparison of
// text would see damage in every check and restore the rules again and again. Fewer rules than
// wanted is damage; as many or more is not. Another program may add a rule to a chain (an
// administrator blocking an address in a hurry), or the host's iptables may split a rule in
// several, and neither keeps the agent's rules from doing their work; replacing the chain at every
// check would take such a rule away at the next 30 seconds, and fight with whatever adds it back.
// The next full restore of the rules does replace the chain (it is the agent's).
//
// The check costs no command of its own: it reads the rules of each chain in the same `-S` that
// tells whether the chain exists.
func (f *Firewall) Missing(ctx context.Context, jumps []Jump, chains map[string][]string, want map[string]int) []string {
	var missing []string
	for _, j := range f.withDocker(ctx, jumps) {
		if f.iptables(ctx, j.args("-C")...) != nil {
			missing = append(missing, "jump "+j.Table+"/"+j.Chain+" -> "+j.Target)
		}
	}
	for _, table := range tableNames {
		for _, chain := range chains[table] {
			out, err := f.iptablesOut(ctx, "-t", table, "-S", chain)
			if err != nil {
				missing = append(missing, "chain "+table+"/"+chain)
				continue
			}
			if wanted, ok := want[table+"/"+chain]; ok {
				if held := countIn(out, chain); held < wanted {
					missing = append(missing, fmt.Sprintf("chain %s/%s holds %d of %d rules", table, chain, held, wanted))
				}
			}
		}
	}
	return missing
}

// Remove deletes the jumps (also from DOCKER-USER), then flushes and deletes the chains
// (table -> chains).
func (f *Firewall) Remove(ctx context.Context, jumps []Jump, chains map[string][]string) {
	for _, j := range append(slices.Clone(jumps), dockerCopies(jumps)...) {
		for i := 0; i < 8 && f.iptables(ctx, j.args("-D")...) == nil; i++ {
		}
	}
	for _, table := range tableNames {
		for _, chain := range chains[table] {
			_ = f.iptables(ctx, "-t", table, "-F", chain)
		}
		for _, chain := range chains[table] {
			_ = f.iptables(ctx, "-t", table, "-X", chain)
		}
	}
}
