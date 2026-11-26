// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

//go:build !linux

package meshagent

import "os"

// The agent runs on Linux only (see device_other.go), so there is never another one to lock out or
// to stop: the lock is always free, and no process is an agent.
func newOSLock() osLock {
	return osLock{
		tryLock:   func(*os.File) (bool, error) { return true, nil },
		unlock:    func(*os.File) {},
		isAgent:   func(int) bool { return false },
		terminate: func(int) error { return errLinuxOnly },
		pid:       os.Getpid,
	}
}
