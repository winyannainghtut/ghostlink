// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

//go:build linux

package meshagent

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func newOSLock() osLock {
	return osLock{tryLock: flockTry, unlock: flockUnlock, isAgent: processIsAgent, terminate: terminateProcess, pid: os.Getpid}
}

// flock runs unix.Flock on f, again when a signal interrupts it.
func flock(f *os.File, how int) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var result error
	if err := raw.Control(func(fd uintptr) {
		for {
			if result = unix.Flock(int(fd), how); !errors.Is(result, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return err
	}
	return result
}

// flockTry takes an exclusive flock without waiting. flock locks belong to the open file, not to the
// process, so a second open of the same file in the same process is refused too.
func flockTry(f *os.File) (bool, error) {
	switch err := flock(f, unix.LOCK_EX|unix.LOCK_NB); {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}

func flockUnlock(f *os.File) { _ = flock(f, unix.LOCK_UN) }

// processIsAgent reads the command line of the process from /proc.
func processIsAgent(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	return isAgentCommand(strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"))
}

func terminateProcess(pid int) error { return unix.Kill(pid, unix.SIGTERM) }
