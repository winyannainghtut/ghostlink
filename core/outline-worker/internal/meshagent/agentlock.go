// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package meshagent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ghostlink.local/outline-worker/internal/hostfw"
)

// lockFile is in the state directory. The running agent (`ghostlink-mesh serve`) holds an
// exclusive lock on it for its whole life and writes its process ID in it. The lock is flock on
// Linux, so the kernel drops it when the process ends, however it ends (a crash, kill -9, a power
// cut): a pid left in the file by such an end tells nothing, only a held lock does.
//
// `ghostlink-mesh leave` uses it to ask the agent to stop and to wait until it has (see
// stopAgent), then holds the lock itself while it takes the node off the machine, so an agent that
// is started meanwhile (systemd's Restart=on-failure, a hand) waits for it and then finds the
// identity gone.
//
// The file is never deleted: removing a lock file that another process may be about to open lets
// two processes hold "the" lock on two different files.
const lockFile = "agent.lock"

var (
	// agentStopWait is how long `leave` waits for a running agent that it asked to stop. The agent
	// takes everything it set up down before it ends (a second or two); after this long `leave`
	// goes on without it, which is safe: the agent stops by itself as soon as it sees the identity
	// gone (see Agent.Serve).
	agentStopWait = 30 * time.Second
	// agentStartWait is how long a starting agent waits for the lock: a `leave` that is just
	// finishing holds it while it tells the coordinator (up to about 10 s to reach it).
	agentStartWait = 15 * time.Second
	// lockPoll is the time between two tries for the lock.
	lockPoll = 100 * time.Millisecond
)

// osLock is what the agent lock needs from the operating system. The Linux one (lock_linux.go) is
// flock, /proc and kill. Elsewhere there is no agent to lock out (the agent runs on Linux only) and
// nothing is ever held. Tests supply a stand-in, so the logic is tried on every platform.
type osLock struct {
	// tryLock takes an exclusive lock on f without waiting. It reports false, with no error, when
	// another open file holds the lock.
	tryLock func(f *os.File) (bool, error)
	// unlock gives the lock back (closing the file does too).
	unlock func(f *os.File)
	// isAgent reports whether the process is a `ghostlink-mesh serve`.
	isAgent func(pid int) bool
	// terminate asks the process to stop (SIGTERM).
	terminate func(pid int) error
	// pid is this process's ID.
	pid func() int
}

var osLocks = newOSLock()

// AgentRunningError says that another agent holds the lock of the state directory.
type AgentRunningError struct{ PID int }

func (e *AgentRunningError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("another ghostlink-mesh agent is already running for this node (process %d); stop it first: sudo systemctl stop ghostlink-mesh", e.PID)
	}
	return "another ghostlink-mesh agent is already running for this node; stop it first: sudo systemctl stop ghostlink-mesh"
}

// AgentLock is a held lock of the state directory.
type AgentLock struct {
	f   *os.File
	sys osLock
}

// Release empties the file of the process ID and gives the lock back. It is safe on a nil lock.
func (l *AgentLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Truncate(0)
	l.sys.unlock(l.f)
	_ = l.f.Close()
	l.f = nil
}

func openLockFile(dir string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
}

// readPID reads the process ID in the lock file, 0 when there is none. The file may be read while
// another process holds the lock: flock only stops other flocks.
func readPID(f *os.File) int {
	buf := make([]byte, 24)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid < 0 {
		return 0
	}
	return pid
}

// recordPID writes the process ID, replacing what was there. It comes right after the lock was
// taken, so the old content (the pid of an agent that ended) is shortly all that can be read.
func recordPID(f *os.File, pid int) {
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(pid)+"\n"), 0)
	}
}

// LockAgent takes the lock for a running agent, waiting up to wait for another holder to let go,
// and records this process's ID. It returns an *AgentRunningError when the lock stays held, and
// ctx's error when ctx ends first.
func LockAgent(ctx context.Context, dir string, wait time.Duration) (*AgentLock, error) {
	return lockAgent(ctx, osLocks, dir, wait)
}

func lockAgent(ctx context.Context, sys osLock, dir string, wait time.Duration) (*AgentLock, error) {
	f, err := openLockFile(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot open the agent lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		got, err := sys.tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cannot lock %s: %w", f.Name(), err)
		}
		if got {
			break
		}
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			pid := readPID(f)
			_ = f.Close()
			return nil, &AgentRunningError{PID: pid}
		}
		sleep(ctx, lockPoll)
	}
	recordPID(f, sys.pid())
	return &AgentLock{f: f, sys: sys}, nil
}

// isAgentCommand reports whether a command line (as /proc/PID/cmdline gives it) is that of
// `ghostlink-mesh serve`, however it was installed or started: the program is named ghostlink-mesh
// and its first argument is serve (main reads the command from os.Args[1]).
func isAgentCommand(args []string) bool {
	return len(args) >= 2 && path.Base(args[0]) == "ghostlink-mesh" && args[1] == "serve"
}

// stopAgent makes sure no agent of dir is running, and returns the lock, held, so that none
// starts while the caller takes the node off the machine. A running agent is asked to stop with
// SIGTERM, once, after the process named in the lock file was checked to be a `ghostlink-mesh
// serve`; the lock is then free when the agent has ended, which is after it took down what it set
// up. The wait is bounded: when the agent does not let go in time, or the lock cannot be used at
// all, it returns nil and the caller goes on, as it did before there was a lock (the agent then
// stops by itself when it sees the identity gone). With no agent running it returns at once.
func stopAgent(sys osLock, dir string, wait time.Duration, say func(format string, args ...any)) *AgentLock {
	f, err := openLockFile(dir)
	if err != nil {
		say("cannot use the agent lock (%v); going on without it", err)
		return nil
	}
	deadline := time.Now().Add(wait)
	signalled := map[int]bool{}
	for {
		got, err := sys.tryLock(f)
		if err != nil {
			say("cannot lock %s (%v); going on without it", f.Name(), err)
			_ = f.Close()
			return nil
		}
		if got {
			// What is left in the file is from an agent that ended without cleaning up.
			_ = f.Truncate(0)
			return &AgentLock{f: f, sys: sys}
		}
		// Something holds the lock. Only the process named in the file, and only when it really is an
		// agent, is asked to stop: a stale number could be any process by now, and it is looked at
		// again each time, since an agent that is just starting writes its number a moment after it
		// took the lock.
		if pid := readPID(f); pid > 1 && pid != sys.pid() && !signalled[pid] && sys.isAgent(pid) {
			signalled[pid] = true
			say("stopping the running agent (process %d)", pid)
			if err := sys.terminate(pid); err != nil {
				say("could not ask process %d to stop: %v", pid, err)
			}
		}
		if !time.Now().Before(deadline) {
			say("the agent did not stop within %s; removing the node anyway (the agent stops by itself once it sees the identity gone)", wait)
			_ = f.Close()
			return nil
		}
		time.Sleep(lockPoll)
	}
}

// Run is `ghostlink-mesh serve`: it runs the agent for the node in dir, holding the agent lock the
// whole time, until ctx ends (SIGTERM or Ctrl-C), the node is removed, or `leave` took it off the
// machine. Ending because ctx ended, or because the node left, is not an error: the process exits
// with status 0, so systemd (Restart=on-failure) does not start it again.
//
// Nothing is created before the identity is known to be there: started on a machine that is not in
// a mesh (or that just left), it returns ErrNotJoined and leaves the state directory as it was. It
// never joins or sets up anything by itself.
func Run(ctx context.Context, dir string, newFirewall func() *hostfw.Firewall, version string, logger *log.Logger) error {
	return run(ctx, osLocks, dir, newFirewall, version, logger)
}

func run(ctx context.Context, sys osLock, dir string, newFirewall func() *hostfw.Firewall, version string, logger *log.Logger) error {
	if _, err := LoadNode(dir); err != nil {
		return err
	}
	lock, err := lockAgent(ctx, sys, dir, agentStartWait)
	if err != nil {
		if ctx.Err() != nil {
			return nil // asked to stop while waiting
		}
		return err
	}
	defer lock.Release()
	// `leave` may have finished while the lock was waited for: the identity it removed is not set up
	// again.
	node, err := LoadNode(dir)
	if errors.Is(err, ErrNotJoined) {
		logger.Printf("this node left the mesh; nothing to run")
		return nil
	}
	if err != nil {
		return err
	}
	agent := &Agent{Dir: dir, Node: node, Log: logger, FW: newFirewall(), Version: version}
	logger.Printf("starting %s (%s) for %s", node.Name, node.Address, node.URL)
	if err := agent.Serve(ctx); err != nil {
		return err
	}
	logger.Printf("stopped")
	return nil
}
