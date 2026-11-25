// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package main

import (
	"context"
	"slices"
	"testing"
	"time"
)

type stopFunc func()

func (f stopFunc) Close()                         { f() }
func (f stopFunc) Shutdown(context.Context) error { f(); return nil }

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

// The accounting loop makes its last pass before the dashboard and the mesh listener are closed:
// that pass can save a change whose hooks move those listeners to match it, and one that is closed
// already would be opened again. Before, they were closed first and the loop waited for after.
func TestStopServingEndsTheAccountingLoopBeforeTheListeners(t *testing.T) {
	var steps []string
	accounted := make(chan struct{})
	ended := func() bool {
		select {
		case <-accounted:
			return true
		default:
			return false
		}
	}
	listener := func(name string) stopFunc {
		return func() {
			if !ended() {
				t.Errorf("the %s was closed while the accounting loop was still running", name)
			}
			steps = append(steps, name)
		}
	}
	api := stopFunc(func() { steps = append(steps, "api") })
	engine := closeFunc(func() error {
		steps = append(steps, "engine")
		return nil
	})
	// The loop is still working on its last pass when the API has stopped.
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(accounted)
	}()
	stopServing(api, accounted, listener("dashboard"), listener("mesh listener"), engine)
	if want := []string{"api", "dashboard", "mesh listener", "engine"}; !slices.Equal(steps, want) {
		t.Fatalf("stopped in the order %v, want %v", steps, want)
	}
}
