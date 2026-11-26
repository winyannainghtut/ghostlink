// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"ghostlink.local/outline-worker/internal/meshagent"
)

const runMainEnv = "GHOSTLINK_MESH_TEST_RUN_MAIN"

// TestMain lets the test binary run as the program itself: with runMainEnv set it runs main with
// the arguments it was given, so a test can start `ghostlink-mesh serve` as a process and look at
// how it ends.
func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) != "" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// `ghostlink-mesh leave` stops a running agent with SIGTERM, and the agent must then end with status
// 0: systemd's Restart=on-failure starts a process that failed (a non-zero status), not one that
// ended by itself. The agent takes down what it set up first, gives the lock back, and keeps the
// identity: removing it is leave's work.
func TestServeExitsWithStatusZeroWhenAskedToStop(t *testing.T) {
	dir := t.TempDir()
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	node := &meshagent.Node{URL: "https://192.0.2.1:8443", Pin: strings.Repeat("0", 64), NodeID: "1", Secret: "1.secret",
		PrivateKey: key.String(), Name: "alpha", Address: "100.96.0.5", Interface: "gltest9", ListenPort: 51820}
	if err := node.Save(dir); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	cmd := exec.Command(exe, "serve", "--state", dir, "--iptables", "nft")
	// A PATH with no programs in it: the clean-up the agent does when it stops finds no iptables,
	// so it changes nothing on this machine.
	cmd.Env = []string{runMainEnv + "=1", "PATH=" + t.TempDir()}
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()

	lock := filepath.Join(dir, "agent.lock")
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if data, _ := os.ReadFile(lock); strings.TrimSpace(string(data)) == strconv.Itoa(cmd.Process.Pid) {
			break
		}
		select {
		case err := <-ended:
			t.Fatalf("the agent ended before it was asked to: %v\n%s", err, logs)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not take the lock\n%s", logs)
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the agent asked to stop did not end with status 0 (%v)\n%s", err, logs)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the agent did not stop\n%s", logs)
	}
	if data, _ := os.ReadFile(lock); len(data) != 0 {
		t.Errorf("a process ID is left in the lock file: %q", data)
	}
	if _, err := meshagent.LoadNode(dir); err != nil {
		t.Errorf("the identity did not survive a stop: %v", err)
	}
	if !strings.Contains(logs.String(), "stopped") {
		t.Errorf("the agent does not log that it stopped:\n%s", logs)
	}
}

// Started on a machine that is not in a mesh, the agent fails with a message and creates nothing.
func TestServeWithoutAnIdentityCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	cmd := exec.Command(exe, "serve", "--state", dir)
	cmd.Env = []string{runMainEnv + "=1", "PATH=" + t.TempDir()}
	cmd.Stderr = logs
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(logs.String(), "not in a mesh") {
		t.Fatalf("serve without an identity: %v\n%s", err, logs)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("serve created %d files in the state directory", len(entries))
	}
}
