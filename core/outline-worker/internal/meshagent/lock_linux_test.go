// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

//go:build linux

package meshagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// These tests use the real flock, /proc and kill, with processes of their own: the test binary
// starts itself under the name `ghostlink-mesh serve` (see TestMain) as a stand-in for the agent.

const (
	helperDirEnv  = "GHOSTLINK_MESH_TEST_AGENT_DIR"
	helperModeEnv = "GHOSTLINK_MESH_TEST_AGENT_MODE"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(helperDirEnv); dir != "" {
		os.Exit(helperAgent(dir, os.Getenv(helperModeEnv)))
	}
	os.Exit(m.Run())
}

// helperAgent is the stand-in agent. In "hold" mode it takes the agent lock, as `serve` does, and
// when asked to stop (SIGTERM) takes a moment to "take down what it set up", gives the lock back
// and ends with status 0. In "idle" mode it holds nothing, whatever its command line says, and ends
// with status 3 if it is ever asked to stop.
func helperAgent(dir, mode string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	if mode == "hold" {
		lock, err := LockAgent(ctx, dir, time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "stand-in agent:", err)
			return 1
		}
		fmt.Println("ready")
		<-ctx.Done()
		time.Sleep(300 * time.Millisecond)
		lock.Release()
		return 0
	}
	fmt.Println("ready")
	select {
	case <-ctx.Done():
		return 3
	case <-time.After(60 * time.Second):
		return 4
	}
}

// startHelper starts the stand-in agent as `ghostlink-mesh serve --state DIR` and waits until it is
// ready. It is killed when the test ends, if it still runs.
func startHelper(t *testing.T, dir, mode string) (*exec.Cmd, <-chan error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: exe, Args: []string{"ghostlink-mesh", "serve", "--state", dir},
		Env: append(os.Environ(), helperDirEnv+"="+dir, helperModeEnv+"="+mode), Stderr: os.Stderr}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- line
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("the stand-in agent said %q", line)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stand-in agent did not start")
	}
	go func() { ended <- cmd.Wait() }()
	return cmd, ended
}

// The real lock: a second holder is refused while the first one has it (flock locks belong to the
// open file, so this holds within one process too), the refusal names the first one, and when the
// first lets go the lock can be taken again.
func TestRealLockRefusesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := LockAgent(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := pidFile(t, dir); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("the lock file holds %q, want this process's ID", got)
	}
	var running *AgentRunningError
	if _, err := LockAgent(context.Background(), dir, 0); !errors.As(err, &running) || running.PID != os.Getpid() {
		t.Fatalf("a second holder: %v", err)
	}
	first.Release()
	if got := pidFile(t, dir); got != "" {
		t.Errorf("the lock file still holds %q after the lock was given back", got)
	}
	again, err := LockAgent(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("the lock was not free after Release: %v", err)
	}
	again.Release()
}

// A real process that holds the real lock is recognised from /proc, asked to stop with a real
// SIGTERM, and waited for: Leave returns only after the agent let go, and the agent ended with
// status 0, which is what keeps systemd (Restart=on-failure) from starting it again.
func TestLeaveStopsARealAgentProcess(t *testing.T) {
	j := newJoined(t)
	cmd, ended := startHelper(t, j.dir, "hold")
	pid := cmd.Process.Pid
	if got := pidFile(t, j.dir); got != strconv.Itoa(pid) {
		t.Fatalf("the agent recorded %q, want its process ID %d", got, pid)
	}
	if !processIsAgent(pid) || processIsAgent(os.Getpid()) {
		t.Fatal("/proc does not tell the agent from this test")
	}

	started := time.Now()
	if err := Leave(j.dir, j.fw); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Errorf("Leave returned after %v, before the agent had finished taking things down", elapsed)
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the agent did not end with status 0: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent is still running after Leave")
	}
	j.gone(t)
	if got := pidFile(t, j.dir); got != "" {
		t.Errorf("a process ID is left in the lock file: %q", got)
	}
}

// A process ID in the file with nobody holding the lock (the agent was killed, and the number is
// another agent's by now) is not signalled, even when the process looks exactly like an agent.
func TestLeaveDoesNotSignalAProcessThatHoldsNoLock(t *testing.T) {
	j := newJoined(t)
	cmd, ended := startHelper(t, j.dir, "idle")
	writePID(t, j.dir, strconv.Itoa(cmd.Process.Pid)+"\n")
	if !processIsAgent(cmd.Process.Pid) {
		t.Fatal("the stand-in does not look like an agent")
	}
	if err := Leave(j.dir, j.fw); err != nil {
		t.Fatal(err)
	}
	j.gone(t)
	select {
	case err := <-ended:
		t.Fatalf("Leave signalled a process that held no lock: it ended with %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	_ = cmd.Process.Kill()
	<-ended
}

// An agent that is killed (no clean-up, no chance to give the lock back) does not hold it: the
// kernel drops the lock with the process, and the next agent, or leave, takes it at once.
func TestKilledAgentDoesNotHoldTheLock(t *testing.T) {
	dir := t.TempDir()
	cmd, ended := startHelper(t, dir, "hold")
	var running *AgentRunningError
	if _, err := LockAgent(context.Background(), dir, 0); !errors.As(err, &running) || running.PID != cmd.Process.Pid {
		t.Fatalf("the lock of a running agent: %v", err)
	}
	_ = cmd.Process.Kill()
	<-ended
	lock, err := LockAgent(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("the lock of a killed agent is still held: %v", err)
	}
	defer lock.Release()
	if got := pidFile(t, dir); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("the dead agent's process ID was not replaced: %q", got)
	}
}
