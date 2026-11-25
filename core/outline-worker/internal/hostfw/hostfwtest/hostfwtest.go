// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

// Package hostfwtest is a pretend host for hostfw: it keeps the tables, chains and rules that
// iptables would, understands the commands hostfw and the agents run (-C, -I, -A, -D, -S, -F, -X,
// -N, and iptables-restore --noflush), and can lose its rules the way a firewall reload or
// iptables -F makes a host lose them. Tests give hostfw.Firewall.Exec to Host.Exec, so what an
// agent does after a reload can be tried on any machine, without iptables or root. It is not an
// iptables implementation: it knows only what the agents use, and rules are compared as the text
// they were given in.
package hostfwtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var builtin = map[string][]string{
	"filter": {"INPUT", "FORWARD", "OUTPUT"},
	"nat":    {"PREROUTING", "INPUT", "OUTPUT", "POSTROUTING"},
	"mangle": {"PREROUTING", "INPUT", "FORWARD", "OUTPUT", "POSTROUTING"},
}

// targets are the rule targets that are not chains.
var targets = []string{"ACCEPT", "DROP", "REJECT", "RETURN", "MASQUERADE", "DNAT", "SNAT", "REDIRECT", "TCPMSS", "LOG", "MARK", "CONNMARK"}

type chain struct {
	builtin bool
	rules   []string // each as its spec: what follows "-A CHAIN"
}

// Host is a fake host's firewall. The zero value is not ready: use New.
type Host struct {
	mu     sync.Mutex
	tables map[string]map[string]*chain
	calls  []string
	fail   func(name string, args []string) error
}

// New returns a host with the built-in chains and no rules.
func New() *Host {
	h := &Host{tables: map[string]map[string]*chain{}}
	for table, names := range builtin {
		h.tables[table] = map[string]*chain{}
		for _, name := range names {
			h.tables[table][name] = &chain{builtin: true}
		}
	}
	return h
}

// Fail makes every command for which fn returns an error fail with it; nil stops that. fn sees
// the command's name (iptables-nft, iptables-nft-restore...) and arguments, and must not call
// the Host.
func (h *Host) Fail(fn func(name string, args []string) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fail = fn
}

// AddChain creates a chain, as other software does (Docker's DOCKER-USER, for example).
func (h *Host) AddChain(table, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tables[table][name] = &chain{}
}

// Flush is iptables -F: every rule in every chain goes, the chains stay (the jumps to the agent's
// chains were rules in the built-in chains, so they go too).
func (h *Host) Flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, chains := range h.tables {
		for _, c := range chains {
			c.rules = nil
		}
	}
}

// Reset is iptables -F followed by iptables -X, or a reload that replaces the whole ruleset
// (netfilter-persistent, iptables-restore without --noflush): nothing of the agent is left, and
// no user chain either.
func (h *Host) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, chains := range h.tables {
		for name, c := range chains {
			if c.builtin {
				c.rules = nil
			} else {
				delete(chains, name)
			}
		}
	}
}

// Delete removes the first rule equal to spec from a chain, as iptables -D does; it reports
// whether there was one.
func (h *Host) Delete(table, name, spec string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.tables[table][name]
	if c == nil {
		return false
	}
	i := slices.Index(c.rules, spec)
	if i < 0 {
		return false
	}
	c.rules = slices.Delete(c.rules, i, i+1)
	return true
}

// Has reports whether the chain exists.
func (h *Host) Has(table, name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tables[table][name] != nil
}

// Rules are the rules of a chain, each as what follows "-A CHAIN"; nil if there are none or no
// such chain.
func (h *Host) Rules(table, name string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.tables[table][name]; c != nil {
		return slices.Clone(c.rules)
	}
	return nil
}

// Chains lists the chains that are not built in, as "table/CHAIN", sorted.
func (h *Host) Chains() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for table, chains := range h.tables {
		for name, c := range chains {
			if !c.builtin {
				out = append(out, table+"/"+name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Calls are the commands run so far, each as its name and arguments.
func (h *Host) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

// Count is the number of commands run so far that contain text.
func (h *Host) Count(text string) int {
	n := 0
	for _, call := range h.Calls() {
		if strings.Contains(call, text) {
			n++
		}
	}
	return n
}

// Exec is hostfw.Exec: it runs one command on the pretend host.
func (h *Host) Exec(_ context.Context, stdin, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	if h.fail != nil {
		if err := h.fail(name, args); err != nil {
			return "", err
		}
	}
	switch name {
	case "iptables-nft", "iptables-legacy":
		return h.command(args)
	case "iptables-nft-restore", "iptables-legacy-restore":
		return "", h.restore(stdin)
	}
	return "", fmt.Errorf("%s: executable file not found in $PATH", name)
}

func (h *Host) command(args []string) (string, error) {
	for len(args) >= 2 && args[0] == "-w" {
		args = args[2:]
	}
	table := "filter"
	if len(args) >= 2 && args[0] == "-t" {
		table, args = args[1], args[2:]
	}
	chains := h.tables[table]
	if chains == nil || len(args) < 2 {
		return "", errors.New("iptables: bad command line")
	}
	op, name, rest := args[0], args[1], args[2:]
	c := chains[name]
	if op == "-N" {
		if c != nil {
			return "", errors.New("iptables: Chain already exists.")
		}
		chains[name] = &chain{}
		return "", nil
	}
	if c == nil {
		return "", errors.New("iptables: No chain/target/match by that name.")
	}
	switch op {
	case "-C":
		if !slices.Contains(c.rules, strings.Join(rest, " ")) {
			return "", errors.New("iptables: Bad rule (does a matching rule exist in that chain?).")
		}
	case "-D":
		i := slices.Index(c.rules, strings.Join(rest, " "))
		if i < 0 {
			return "", errors.New("iptables: Bad rule (does a matching rule exist in that chain?).")
		}
		c.rules = slices.Delete(c.rules, i, i+1)
	case "-A", "-I":
		position := len(c.rules)
		if op == "-I" {
			position = 0
			if len(rest) > 0 {
				if n, err := strconv.Atoi(rest[0]); err == nil {
					position, rest = max(n-1, 0), rest[1:]
				}
			}
		}
		spec := strings.Join(rest, " ")
		if err := checkTarget(chains, spec); err != nil {
			return "", err
		}
		c.rules = slices.Insert(c.rules, min(position, len(c.rules)), spec)
	case "-F":
		c.rules = nil
	case "-X":
		if c.builtin {
			return "", errors.New("iptables: Cannot delete a built-in chain.")
		}
		if len(c.rules) > 0 || referenced(chains, name) {
			return "", errors.New("iptables: Too many links.")
		}
		delete(chains, name)
	case "-S":
		var out strings.Builder
		if c.builtin {
			fmt.Fprintf(&out, "-P %s ACCEPT\n", name)
		} else {
			fmt.Fprintf(&out, "-N %s\n", name)
		}
		for _, rule := range c.rules {
			fmt.Fprintf(&out, "-A %s %s\n", name, rule)
		}
		return out.String(), nil
	default:
		return "", fmt.Errorf("iptables: unsupported operation %s", op)
	}
	return "", nil
}

// target is the chain a rule jumps to, or "" when it ends in a built-in target.
func target(spec string) string {
	fields := strings.Fields(spec)
	for i, f := range fields {
		if (f == "-j" || f == "-g") && i+1 < len(fields) && !slices.Contains(targets, fields[i+1]) {
			return fields[i+1]
		}
	}
	return ""
}

func checkTarget(chains map[string]*chain, spec string) error {
	if t := target(spec); t != "" && chains[t] == nil {
		return errors.New("iptables: No chain/target/match by that name.")
	}
	return nil
}

func referenced(chains map[string]*chain, name string) bool {
	for _, c := range chains {
		for _, rule := range c.rules {
			if target(rule) == name {
				return true
			}
		}
	}
	return false
}

// restore is iptables-restore --noflush: each table is changed all at once or not at all; a
// declared chain (":NAME - [0:0]") is created, or emptied if it exists; other chains and the
// other rules stay.
func (h *Host) restore(input string) error {
	var table string
	var work map[string]*chain
	for number, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(line)
		fail := func(why string) error { return fmt.Errorf("iptables-restore: line %d failed: %s", number+1, why) }
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "*"):
			table = line[1:]
			if h.tables[table] == nil {
				return fail("unknown table " + table)
			}
			work = map[string]*chain{}
			for name, c := range h.tables[table] {
				work[name] = &chain{builtin: c.builtin, rules: slices.Clone(c.rules)}
			}
		case work == nil:
			return fail("no table selected")
		case strings.HasPrefix(line, ":"):
			name, _, _ := strings.Cut(line[1:], " ")
			if c := work[name]; c != nil {
				c.rules = nil
			} else {
				work[name] = &chain{}
			}
		case strings.HasPrefix(line, "-A "):
			name, spec, _ := strings.Cut(line[3:], " ")
			c := work[name]
			if c == nil {
				return fail("no chain " + name)
			}
			if err := checkTarget(work, spec); err != nil {
				return fail(err.Error())
			}
			c.rules = append(c.rules, spec)
		case line == "COMMIT":
			h.tables[table], work = work, nil
		default:
			return fail("cannot understand " + line)
		}
	}
	if work != nil {
		return errors.New("iptables-restore: COMMIT expected")
	}
	return nil
}
