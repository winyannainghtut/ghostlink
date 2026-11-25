// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package main

import (
	"strings"
	"testing"
)

// `ghostlink-server mesh join-here` passes --endpoint the address the server's host name resolves
// to. On a test machine (host name 127.0.0.1) or a host whose own name maps to 127.0.1.1 that is a
// loopback address, and the join must still work; the address is just not offered to other nodes.
func TestJoinEndpoint(t *testing.T) {
	for _, c := range []struct{ in, offered string }{
		{"", ""},
		{"198.51.100.7:51820", "198.51.100.7:51820"},
		{"192.168.1.20:41000", "192.168.1.20:41000"},
	} {
		offered, note, err := offeredEndpoint(c.in)
		if err != nil || offered != c.offered || note != "" {
			t.Errorf("%q: offered %q, note %q, error %v", c.in, offered, note, err)
		}
	}
	for _, loopback := range []string{"127.0.0.1:51820", "127.0.1.1:51820"} {
		offered, note, err := offeredEndpoint(loopback)
		if err != nil || offered != "" || !strings.Contains(note, loopback) || !strings.Contains(note, "not offered") {
			t.Errorf("%s: offered %q, note %q, error %v", loopback, offered, note, err)
		}
	}
	for _, bad := range []string{"198.51.100.7", "host:51820", "198.51.100.7:0", "169.254.169.254:51820", "0.0.0.0:51820", "224.0.0.1:51820", "255.255.255.255:51820", "[2001:db8::1]:51820"} {
		if offered, _, err := offeredEndpoint(bad); err == nil || offered != "" {
			t.Errorf("%q was accepted: offered %q", bad, offered)
		}
	}
}
