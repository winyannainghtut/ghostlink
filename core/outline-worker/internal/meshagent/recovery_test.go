// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package meshagent

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/hostfw"
	"ghostlink.local/outline-worker/internal/hostfw/hostfwtest"
	"ghostlink.local/outline-worker/internal/meshproto"
)

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// leftBehind puts on a pretend host what an agent that was killed leaves on a machine: its
// firewall chains and jumps, and its block of names in /etc/hosts (a file of its own here).
func leftBehind(t *testing.T, node *Node, fw *hostfw.Firewall, host *hostfwtest.Host) (hosts string) {
	t.Helper()
	config := &meshproto.Config{MeshCIDR: "100.96.0.0/16", Address: "100.96.0.5/16", Firewall: meshproto.Firewall{Open: true}}
	if err := (&Agent{Node: node, FW: fw}).firewall(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	hosts = filepath.Join(t.TempDir(), "hosts")
	text := RenderHosts("127.0.0.1\tlocalhost\n", []meshproto.Host{{Name: "web", Address: "100.96.0.2"}})
	if err := os.WriteFile(hosts, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := HostsPath
	HostsPath = hosts
	t.Cleanup(func() { HostsPath = previous })
	if len(host.Chains()) == 0 || !strings.Contains(readText(t, hosts), hostsBegin) {
		t.Fatal("nothing was left behind")
	}
	return hosts
}

// Taking the names down must not touch a hosts file that has none of the agent's: it is done at
// every start of a node that is waiting or disabled, and /etc/hosts may be missing, or a bind mount
// that cannot be written.
func TestTakingNamesDownLeavesAHostsFileWithoutThemAlone(t *testing.T) {
	previous := HostsPath
	t.Cleanup(func() { HostsPath = previous })
	dir := t.TempDir()
	HostsPath = filepath.Join(dir, "hosts")
	if err := writeHosts(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(HostsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a hosts file was made where there was none: %v", err)
	}
	// Odd line endings stay as they are: the file is not rewritten.
	plain := "127.0.0.1\tlocalhost\n\n\n# no newline at the end"
	if err := os.WriteFile(HostsPath, []byte(plain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeHosts(nil); err != nil || readText(t, HostsPath) != plain {
		t.Fatalf("a file without the agent's block was changed: %q %v", readText(t, HostsPath), err)
	}
	// With the block, it goes.
	with := RenderHosts(plain, []meshproto.Host{{Name: "web", Address: "100.96.0.2"}})
	if err := os.WriteFile(HostsPath, []byte(with), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeHosts(nil); err != nil || strings.Contains(readText(t, HostsPath), hostsBegin) || !strings.Contains(readText(t, HostsPath), "localhost") {
		t.Fatalf("the block was not taken out: %q %v", readText(t, HostsPath), err)
	}
	// And the names are written when there are some.
	if err := writeHosts([]meshproto.Host{{Name: "db", Address: "100.96.0.3"}}); err != nil || !strings.Contains(readText(t, HostsPath), "db.mesh") {
		t.Fatalf("names not written: %q %v", readText(t, HostsPath), err)
	}
}

// An agent that was killed (a crash, the out-of-memory killer, a power cut) leaves its interface,
// routes, firewall and names behind. Started again, the new process knows nothing of them, so
// when the coordinator says the node is waiting for approval or disabled it takes them down: it
// must not report a node that is not allowed in while the mesh is still open to it.
func TestPausedNodeRemovesWhatAnEarlierRunLeft(t *testing.T) {
	for _, status := range []string{meshproto.StatusPending, meshproto.StatusDisabled} {
		t.Run(status, func(t *testing.T) {
			node, syncs := fakeCoordinator(t, status)
			node.Hosts = true
			dir := t.TempDir()
			if err := node.Save(dir); err != nil {
				t.Fatal(err)
			}
			host := hostfwtest.New()
			fw := &hostfw.Firewall{Backend: "nft", Exec: host.Exec}
			hosts := leftBehind(t, node, fw, host)

			a := &Agent{Dir: dir, Node: node, Log: log.New(io.Discard, "", 0), FW: fw, Version: "test"}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- a.Serve(ctx) }()
			gone := func() bool {
				return len(host.Chains()) == 0 && !strings.Contains(readText(t, hosts), hostsBegin)
			}
			// An answer that comes at once makes the agent wait 10 seconds before it asks again, so
			// whatever is removed in the first seconds is removed by the agent while it runs, not
			// by the clean-up it does when it stops.
			for deadline := time.Now().Add(5 * time.Second); !gone() && time.Now().Before(deadline); {
				time.Sleep(20 * time.Millisecond)
			}
			removedWhileRunning, stillThere := gone(), host.Chains()
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !removedWhileRunning {
				t.Fatalf("the agent was told the node is %s and left the earlier agent's chains %v or names in /etc/hosts in place", status, stillThere)
			}
			if syncs.Load() != 1 {
				t.Fatalf("%d syncs", syncs.Load())
			}
		})
	}
}

// The agent does that once, at the first answer, and not at every one: asking again and again
// while a node waits for approval must not keep taking the same things down.
func TestSweepOfAnEarlierRunIsDoneOnce(t *testing.T) {
	// An answer that takes over a second is not from an old coordinator (which answers at once), so
	// the agent asks again right away.
	node, syncs := fakeCoordinatorFunc(t, func(int) meshproto.SyncResponse {
		time.Sleep(1100 * time.Millisecond)
		return meshproto.SyncResponse{Status: meshproto.StatusPending}
	})
	dir := t.TempDir()
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	host := hostfwtest.New()
	fw := &hostfw.Firewall{Backend: "nft", Exec: host.Exec}
	a := &Agent{Dir: dir, Node: node, Log: log.New(io.Discard, "", 0), FW: fw, Version: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	for deadline := time.Now().Add(15 * time.Second); syncs.Load() < 3 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	sweeps := host.Count(" -X ") // one per chain each time everything is taken down
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if syncs.Load() < 3 {
		t.Fatalf("only %d syncs", syncs.Load())
	}
	if want := len(Chains()["filter"]) + len(Chains()["nat"]) + len(Chains()["mangle"]); sweeps != want {
		t.Fatalf("%d chains were deleted over %d syncs, want one sweep of %d", sweeps, syncs.Load(), want)
	}
}

// An answer that comes at once (an older coordinator) makes a waiting node pause before it asks
// again; the pause must not depend on how long the agent then takes to take down what an earlier
// run left (a slow firewall takes a while), or it would ask again at once, once.
func TestSlowSweepDoesNotDefeatThePauseBetweenAsks(t *testing.T) {
	node, syncs := fakeCoordinator(t, meshproto.StatusPending)
	dir := t.TempDir()
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	host := hostfwtest.New()
	host.Fail(func(string, []string) error { time.Sleep(60 * time.Millisecond); return nil }) // 23 commands: over a second
	a := &Agent{Dir: dir, Node: node, Log: log.New(io.Discard, "", 0), FW: &hostfw.Firewall{Backend: "nft", Exec: host.Exec}, Version: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if err := a.Serve(ctx); err != nil {
		t.Fatal(err)
	}
	if syncs.Load() != 1 {
		t.Fatalf("%d syncs in 2.5 s while waiting for approval", syncs.Load())
	}
}

// `ghostlink-mesh leave` may finish while the agent's sync is waiting for its answer. What the
// coordinator then answers is not set up on a machine that has just left the mesh, and the agent
// stops at once.
func TestAnswerAfterLeaveIsNotApplied(t *testing.T) {
	dir := t.TempDir()
	config := &meshproto.Config{Generation: 5, Name: "alpha", MeshCIDR: "100.96.0.0/16", Address: "100.96.0.5/16", ListenPort: 51820,
		Peers: []meshproto.Peer{}, Routes: []string{}, Shared: []string{}, Hosts: []meshproto.Host{}, Firewall: meshproto.Firewall{Open: true}}
	node, syncs := fakeCoordinatorFunc(t, func(int) meshproto.SyncResponse {
		Forget(dir) // what the end of `leave` does
		return meshproto.SyncResponse{Status: meshproto.StatusActive, Config: config}
	})
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	host := hostfwtest.New()
	a := &Agent{Dir: dir, Node: node, Log: log.New(io.Discard, "", 0), FW: &hostfw.Firewall{Backend: "nft", Exec: host.Exec}, Version: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	if err := a.Serve(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second || syncs.Load() != 1 {
		t.Fatalf("the agent ran for %v and synced %d times after the node left", elapsed, syncs.Load())
	}
	if host.Count("-restore") != 0 || a.applied != nil {
		t.Fatalf("the agent set the node up again after it left: %v", host.Calls())
	}
	if _, err := os.Stat(filepath.Join(dir, statusFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the status file was written again after the node left: %v", err)
	}
}

// A running agent saves its identity when it learns something (the forwarding setting from
// before). It must not do that after `leave` removed the identity: that would bring the node back
// on a machine that left the mesh.
func TestRunningAgentDoesNotBringBackALeftIdentity(t *testing.T) {
	node, _ := fakeCoordinator(t, meshproto.StatusActive)
	dir := t.TempDir()
	if err := node.saveIfJoined(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("saved an identity that was never there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, nodeFile)); err == nil {
		t.Fatal("the identity was written")
	}
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	node.ForwardingBefore = "1"
	if err := node.saveIfJoined(dir); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadNode(dir); err != nil || loaded.ForwardingBefore != "1" {
		t.Fatalf("the identity was not updated: %+v %v", loaded, err)
	}
	Forget(dir)
	if err := node.saveIfJoined(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("saved the identity after leave: %v", err)
	}
	if _, err := LoadNode(dir); err == nil {
		t.Fatal("the node is back after it left")
	}
}
