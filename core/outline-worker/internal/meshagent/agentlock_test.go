// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package meshagent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/hostfw"
	"ghostlink.local/outline-worker/internal/hostfw/hostfwtest"
	"ghostlink.local/outline-worker/internal/meshproto"
)

// fakeProcesses stands in for the operating system's side of the agent lock: who holds the lock,
// which process IDs are `ghostlink-mesh serve`, and who was asked to stop. "This process" is 100.
type fakeProcesses struct {
	mu         sync.Mutex
	external   bool     // an agent in another process holds the lock
	owner      *os.File // the open file of the lock this process holds
	agents     map[int]bool
	terminated []int
	// onTerminate runs when a process is asked to stop, in the caller's goroutine.
	onTerminate func(pid int)
}

func (h *fakeProcesses) osLock() osLock {
	return osLock{
		tryLock: func(f *os.File) (bool, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.external || (h.owner != nil && h.owner != f) {
				return false, nil
			}
			h.owner = f
			return true, nil
		},
		unlock: func(f *os.File) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.owner == f {
				h.owner = nil
			}
		},
		isAgent: func(pid int) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.agents[pid]
		},
		terminate: func(pid int) error {
			h.mu.Lock()
			h.terminated = append(h.terminated, pid)
			fn := h.onTerminate
			h.mu.Unlock()
			if fn != nil {
				fn(pid)
			}
			return nil
		},
		pid: func() int { return 100 },
	}
}

func (h *fakeProcesses) setExternal(held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.external = held
}

func (h *fakeProcesses) heldByAnAgent() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.external
}

func (h *fakeProcesses) heldByThisProcess() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owner != nil
}

func (h *fakeProcesses) signalled() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.terminated)
}

// quickLocks makes the waits of the agent lock short for a test.
func quickLocks(t *testing.T) {
	t.Helper()
	poll, start := lockPoll, agentStartWait
	lockPoll = 5 * time.Millisecond
	t.Cleanup(func() { lockPoll, agentStartWait = poll, start })
}

func writePID(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, lockFile), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pidFile(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(readText(t, filepath.Join(dir, lockFile)))
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !condition(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// joined is a node that joined a mesh: its identity is saved, its firewall chains are on a pretend
// host, and its coordinator counts what it is asked.
type joined struct {
	dir      string
	node     *Node
	requests *atomic.Int32
	host     *hostfwtest.Host
	fw       *hostfw.Firewall
	logs     *bytes.Buffer
	logger   *log.Logger
}

func newJoined(t *testing.T) *joined {
	t.Helper()
	quickLocks(t)
	node, requests := fakeCoordinator(t, meshproto.StatusActive)
	dir := t.TempDir()
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	host := hostfwtest.New()
	fw := &hostfw.Firewall{Backend: "nft", Exec: host.Exec}
	config := &meshproto.Config{MeshCIDR: "100.96.0.0/16", Address: "100.96.0.5/16", Firewall: meshproto.Firewall{Open: true}}
	if err := (&Agent{Node: node, FW: fw}).firewall(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &joined{dir: dir, node: node, requests: requests, host: host, fw: fw, logs: logs, logger: log.New(logs, "", 0)}
}

// gone fails the test unless the node is off the machine: no identity, no status, no chains, the
// coordinator told.
func (j *joined) gone(t *testing.T) {
	t.Helper()
	for _, name := range []string{nodeFile, statusFile} {
		if _, err := os.Stat(filepath.Join(j.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was left behind (%v)", name, err)
		}
	}
	if left := j.host.Chains(); len(left) != 0 {
		t.Errorf("firewall chains left behind: %v", left)
	}
	if n := j.requests.Load(); n != 1 {
		t.Errorf("the coordinator got %d requests, want the one that tells it the node left", n)
	}
}

// `ghostlink-mesh leave` against a running agent: the agent is asked to stop, and the node is taken
// off the machine only after the agent let go of its lock, which it does when it has taken down
// what it set up. Until then the identity, the chains and the coordinator are as they were.
func TestLeaveAsksTheRunningAgentToStopAndWaitsForIt(t *testing.T) {
	j := newJoined(t)
	h := &fakeProcesses{external: true, agents: map[int]bool{4242: true}}
	writePID(t, j.dir, "4242\n")
	var identity bool
	var requests int32
	var commands int
	before := len(j.host.Calls())
	h.onTerminate = func(int) {
		identity = !left(j.dir)
		requests = j.requests.Load()
		commands = len(j.host.Calls()) - before
		go func() { // the agent takes down what it set up, then lets go
			time.Sleep(80 * time.Millisecond)
			h.setExternal(false)
		}()
	}
	var early atomic.Bool
	j.host.Fail(func(string, []string) error {
		if h.heldByAnAgent() {
			early.Store(true)
		}
		return nil
	})

	started := time.Now()
	if err := leave(j.dir, j.fw, h.osLock(), 10*time.Second, j.logger); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 60*time.Millisecond {
		t.Errorf("leave returned after %v: it did not wait for the agent", elapsed)
	}
	if got := h.signalled(); !slices.Equal(got, []int{4242}) {
		t.Fatalf("processes asked to stop: %v, want [4242] once", got)
	}
	if !identity || requests != 0 || commands != 0 {
		t.Errorf("when the agent was asked to stop the node was already taken apart: identity there %v, %d requests to the coordinator, %d firewall commands", identity, requests, commands)
	}
	if early.Load() {
		t.Error("the firewall was changed while the agent still held the lock")
	}
	j.gone(t)
	if !strings.Contains(j.logs.String(), "stopping the running agent (process 4242)") {
		t.Errorf("leave does not say what it did:\n%s", j.logs)
	}
	if h.heldByThisProcess() || pidFile(t, j.dir) != "" {
		t.Errorf("leave kept the lock or left a process ID: %q", pidFile(t, j.dir))
	}
}

// With no agent running the lock is free, and leave just leaves, as before.
func TestLeaveWithNoAgentJustLeaves(t *testing.T) {
	j := newJoined(t)
	h := &fakeProcesses{agents: map[int]bool{}}
	started := time.Now()
	if err := leave(j.dir, j.fw, h.osLock(), 10*time.Second, j.logger); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("leave took %v with no agent to wait for", elapsed)
	}
	if got := h.signalled(); len(got) != 0 {
		t.Errorf("processes asked to stop with no agent running: %v", got)
	}
	j.gone(t)
	if strings.Contains(j.logs.String(), "agent") {
		t.Errorf("leave talks about an agent when there is none:\n%s", j.logs)
	}
}

// An agent that was killed leaves its process ID in the file, and the number may belong to any
// process by now, even another ghostlink-mesh agent. Only a held lock says an agent runs: without
// one, the number is ignored and nobody is signalled.
func TestLeaveIgnoresAStalePidFile(t *testing.T) {
	j := newJoined(t)
	h := &fakeProcesses{agents: map[int]bool{4242: true}} // the number now belongs to another agent
	writePID(t, j.dir, "4242\n")
	if err := leave(j.dir, j.fw, h.osLock(), 10*time.Second, j.logger); err != nil {
		t.Fatal(err)
	}
	if got := h.signalled(); len(got) != 0 {
		t.Fatalf("a process was signalled on the word of a stale pid file: %v", got)
	}
	j.gone(t)
	if pidFile(t, j.dir) != "" {
		t.Errorf("the stale process ID stayed: %q", pidFile(t, j.dir))
	}
}

// With the lock held, only the process the file names, and only when it is a `ghostlink-mesh serve`,
// is signalled: not another program, not leave itself, not init, and not on an unreadable file.
// Leave still does not wait forever.
func TestLeaveSignalsOnlyAnAgent(t *testing.T) {
	for name, content := range map[string]string{
		"another program": "4242\n",
		"this process":    "100\n",
		"init":            "1\n",
		"a negative":      "-5\n",
		"nothing":         "",
		"text":            "not a number\n",
	} {
		t.Run(name, func(t *testing.T) {
			j := newJoined(t)
			h := &fakeProcesses{external: true, agents: map[int]bool{}}
			writePID(t, j.dir, content)
			if err := leave(j.dir, j.fw, h.osLock(), 150*time.Millisecond, j.logger); err != nil {
				t.Fatal(err)
			}
			if got := h.signalled(); len(got) != 0 {
				t.Fatalf("signalled %v", got)
			}
			j.gone(t)
			if !strings.Contains(j.logs.String(), "did not stop within") {
				t.Errorf("leave does not say that it went on without the agent:\n%s", j.logs)
			}
		})
	}
}

// An agent that ignores the signal does not hold leave up for good: leave asks once, waits the
// bounded time and then removes the node itself (the agent, which sees the identity gone, ends by
// itself).
func TestLeaveDoesNotWaitForeverForAnAgentThatDoesNotStop(t *testing.T) {
	j := newJoined(t)
	h := &fakeProcesses{external: true, agents: map[int]bool{4242: true}}
	writePID(t, j.dir, "4242\n")
	started := time.Now()
	if err := leave(j.dir, j.fw, h.osLock(), 200*time.Millisecond, j.logger); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("leave waited %v for a bound of 200 ms", elapsed)
	}
	if got := h.signalled(); !slices.Equal(got, []int{4242}) {
		t.Errorf("signalled %v, want [4242] once however long it waited", got)
	}
	j.gone(t)
}

// While leave takes the node off the machine it holds the lock: an agent started meanwhile (systemd
// restarts one that crashed just before) cannot get it, and when it does it finds the identity gone.
func TestLeaveHoldsTheLockWhileItRemovesTheNode(t *testing.T) {
	j := newJoined(t)
	h := &fakeProcesses{agents: map[int]bool{}}
	var heldDuring atomic.Bool
	var refused atomic.Bool
	j.host.Fail(func(string, []string) error {
		heldDuring.Store(h.heldByThisProcess())
		if _, err := lockAgent(context.Background(), h.osLock(), j.dir, 0); err != nil {
			var running *AgentRunningError
			refused.Store(errors.As(err, &running))
		}
		return nil
	})
	if err := leave(j.dir, j.fw, h.osLock(), 10*time.Second, j.logger); err != nil {
		t.Fatal(err)
	}
	if !heldDuring.Load() || !refused.Load() {
		t.Fatalf("lock held while the node was taken apart: %v; an agent was refused: %v", heldDuring.Load(), refused.Load())
	}
	lock, err := lockAgent(context.Background(), h.osLock(), j.dir, 0)
	if err != nil {
		t.Fatalf("leave kept the lock: %v", err)
	}
	lock.Release()
}

// A node the coordinator dropped may clean itself up (its agent does, when told) while leave waits
// for the lock; leave then has nothing left to do, and says nothing is wrong.
func TestLeaveFindsTheNodeGoneAfterTheAgentStopped(t *testing.T) {
	j := newJoined(t)
	h := &fakeProcesses{external: true, agents: map[int]bool{4242: true}}
	writePID(t, j.dir, "4242\n")
	h.onTerminate = func(int) {
		Forget(j.dir) // the agent had been told the node was removed, and cleaned up on its way out
		h.setExternal(false)
	}
	if err := leave(j.dir, j.fw, h.osLock(), 10*time.Second, j.logger); err != nil {
		t.Fatalf("leave of a node that left meanwhile: %v", err)
	}
	if n := j.requests.Load(); n != 0 {
		t.Errorf("the coordinator was asked %d times about a node that was already gone", n)
	}
}

// A machine that is not in a mesh: leave says so, and creates nothing (no lock file).
func TestLeaveWithoutAnIdentityCreatesNothing(t *testing.T) {
	quickLocks(t)
	dir := t.TempDir()
	h := &fakeProcesses{agents: map[int]bool{}}
	err := leave(dir, &hostfw.Firewall{}, h.osLock(), time.Second, log.New(&bytes.Buffer{}, "", 0))
	if !errors.Is(err, ErrNotJoined) {
		t.Fatalf("leave of a machine that is not in a mesh: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("leave created %d files", len(entries))
	}
}

func neverFirewall(t *testing.T) func() *hostfw.Firewall {
	return func() *hostfw.Firewall {
		t.Error("the agent started")
		return &hostfw.Firewall{}
	}
}

// `serve` on a machine that is not in a mesh, or that just left (systemd skips the unit then, since
// node.json is gone, but a hand or a restart timer may still start it), must not join, lock or
// create anything: it fails at once and leaves the state directory as it was.
func TestRunWithoutAnIdentityCreatesNothing(t *testing.T) {
	quickLocks(t)
	dir := t.TempDir()
	h := &fakeProcesses{agents: map[int]bool{}}
	err := run(context.Background(), h.osLock(), dir, neverFirewall(t), "test", log.New(&bytes.Buffer{}, "", 0))
	if !errors.Is(err, ErrNotJoined) {
		t.Fatalf("serve on a machine that is not in a mesh: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("serve created %d files", len(entries))
	}
	missing := filepath.Join(dir, "never-made")
	if err := run(context.Background(), h.osLock(), missing, neverFirewall(t), "test", log.New(&bytes.Buffer{}, "", 0)); !errors.Is(err, ErrNotJoined) {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve made the state directory: %v", err)
	}
}

// The agent holds the lock for its whole life and records its process ID; asked to stop (its
// context ends: SIGTERM) it takes down what it set up, gives the lock back, empties the file and
// returns no error, so the process exits with status 0 and systemd does not start it again. It does
// not delete the identity: that is leave's work.
func TestRunHoldsTheLockUntilItStops(t *testing.T) {
	quickLocks(t)
	node, requests := fakeCoordinator(t, meshproto.StatusPending)
	dir := t.TempDir()
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	h := &fakeProcesses{agents: map[int]bool{}}
	host := hostfwtest.New()
	fw := &hostfw.Firewall{Backend: "nft", Exec: host.Exec}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &bytes.Buffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, h.osLock(), dir, func() *hostfw.Firewall { return fw }, "test", log.New(logs, "", 0))
	}()
	eventually(t, "the agent to hold the lock and record its process ID", func() bool {
		return requests.Load() > 0 && h.heldByThisProcess() && pidFile(t, dir) == "100"
	})
	var running *AgentRunningError
	if _, err := lockAgent(context.Background(), h.osLock(), dir, 0); !errors.As(err, &running) || running.PID != 100 {
		t.Fatalf("a second agent was not refused with the first one's process ID: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an agent asked to stop returned an error (the process would exit with status 1 and be started again): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
	}
	if h.heldByThisProcess() || pidFile(t, dir) != "" {
		t.Errorf("the stopped agent kept the lock or its process ID (%q)", pidFile(t, dir))
	}
	if _, err := LoadNode(dir); err != nil {
		t.Errorf("the identity was removed by an agent that was only stopped: %v", err)
	}
	if !strings.Contains(logs.String(), "stopped") {
		t.Errorf("the agent does not log that it stopped:\n%s", logs)
	}
}

// A second agent for the same state is refused, naming the first, and does nothing.
func TestRunRefusesASecondAgent(t *testing.T) {
	quickLocks(t)
	agentStartWait = 100 * time.Millisecond
	node, requests := fakeCoordinator(t, meshproto.StatusPending)
	dir := t.TempDir()
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, "4242\n")
	h := &fakeProcesses{external: true, agents: map[int]bool{4242: true}}
	// The bound is for a run that wrongly starts the agent, which would not return.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := run(ctx, h.osLock(), dir, neverFirewall(t), "test", log.New(&bytes.Buffer{}, "", 0))
	var running *AgentRunningError
	if !errors.As(err, &running) || running.PID != 4242 || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("second agent: %v", err)
	}
	if requests.Load() != 0 {
		t.Error("the refused agent talked to the coordinator")
	}
	if pidFile(t, dir) != "4242" {
		t.Errorf("the refused agent changed the running agent's process ID: %q", pidFile(t, dir))
	}
}

// An agent that starts while `leave` finishes (systemd restarting one that had crashed) waits for
// the lock, and then finds the identity gone: it does not set anything up and does not fail, so the
// service is not started again.
func TestRunThatWaitedForLeaveFindsTheNodeGone(t *testing.T) {
	quickLocks(t)
	node, requests := fakeCoordinator(t, meshproto.StatusActive)
	dir := t.TempDir()
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	h := &fakeProcesses{external: true, agents: map[int]bool{}}
	done := make(chan error, 1)
	logs := &bytes.Buffer{}
	// The bound is for a run that wrongly starts the agent, which would not return.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		done <- run(ctx, h.osLock(), dir, neverFirewall(t), "test", log.New(logs, "", 0))
	}()
	time.Sleep(50 * time.Millisecond)
	Forget(dir) // what the end of leave does, while it holds the lock
	h.setExternal(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an agent that found the node gone failed (systemd would start it again): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not return")
	}
	if requests.Load() != 0 {
		t.Error("the agent talked to the coordinator after the node left")
	}
	if _, err := os.Stat(filepath.Join(dir, nodeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the identity is back: %v", err)
	}
	if h.heldByThisProcess() || pidFile(t, dir) != "" {
		t.Errorf("lock or process ID left behind: %q", pidFile(t, dir))
	}
}

// Asked to stop while it still waits for the lock, an agent ends without an error.
func TestRunAskedToStopWhileWaitingForTheLock(t *testing.T) {
	quickLocks(t)
	node, _ := fakeCoordinator(t, meshproto.StatusActive)
	dir := t.TempDir()
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	h := &fakeProcesses{external: true, agents: map[int]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, h.osLock(), dir, neverFirewall(t), "test", log.New(&bytes.Buffer{}, "", 0)) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an agent stopped while it waited failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept waiting for the lock after it was asked to stop")
	}
}

func TestIsAgentCommand(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"/usr/bin/ghostlink-mesh", "serve"}, true},
		{[]string{"ghostlink-mesh", "serve", "--state", "/var/lib/ghostlink-mesh"}, true},
		{[]string{"/tmp/build/ghostlink-mesh", "serve", "--iptables", "nft"}, true},
		{[]string{"/usr/bin/ghostlink-mesh", "leave"}, false},
		{[]string{"/usr/bin/ghostlink-mesh", "status"}, false},
		{[]string{"/usr/bin/ghostlink-mesh"}, false},
		{[]string{"/usr/bin/ghostlink-server", "serve"}, false},
		{[]string{"/usr/bin/not-ghostlink-mesh", "serve"}, false},
		{[]string{"sleep", "serve"}, false},
		{[]string{"serve"}, false},
		{nil, false},
	} {
		if got := isAgentCommand(c.args); got != c.want {
			t.Errorf("%q: %v, want %v", c.args, got, c.want)
		}
	}
}
