// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package meshproto

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestInvitesRoundTripAndRejectDamage(t *testing.T) {
	inv := Invite{URL: "https://203.0.113.5:14810", Pin: strings.Repeat("AB", 32), Token: "abcdefghijklmnopqrstuvwxyz012345"}
	text := EncodeInvite(inv)
	if !strings.HasPrefix(text, InvitePrefix) || strings.ContainsAny(text, " \n'\"") {
		t.Fatalf("invite is not a single safe word: %q", text)
	}
	got, err := DecodeInvite("  " + text + "\n")
	if err != nil || got != inv {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	bad := []Invite{
		{URL: "http://203.0.113.5:14810", Pin: inv.Pin, Token: inv.Token},
		{URL: "https://x\n", Pin: inv.Pin, Token: inv.Token},
		{URL: inv.URL, Pin: strings.Repeat("ab", 32), Token: inv.Token},
		{URL: inv.URL, Pin: inv.Pin, Token: "short"},
		{URL: inv.URL, Pin: inv.Pin, Token: "abcdefghijklmnop;rm -rf /"},
	}
	for _, b := range bad {
		if _, err := DecodeInvite(EncodeInvite(b)); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
	for _, text := range []string{"", "glmesh1:", "glmesh1:!!!", "ss://abc", InvitePrefix + base64.RawURLEncoding.EncodeToString([]byte("[]"))} {
		if _, err := DecodeInvite(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestNamesKeysEndpointsAndRoutes(t *testing.T) {
	for _, name := range []string{"web", "db-1", "a", strings.Repeat("a", 32)} {
		if !ValidName(name) {
			t.Errorf("refused name %q", name)
		}
	}
	for _, name := range []string{"", "-web", "web-", "Web", "web.mesh", "web_1", "a b", strings.Repeat("a", 33), "x\n"} {
		if ValidName(name) {
			t.Errorf("accepted name %q", name)
		}
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if !ValidKey(key) || ValidKey(key[:43]) || ValidKey(base64.StdEncoding.EncodeToString(make([]byte, 31))) || ValidKey(strings.TrimRight(key, "=")) {
		t.Error("key validation")
	}
	for _, ok := range []string{"198.51.100.7:51820", "10.0.0.1:1"} {
		if ValidEndpoint(ok) != nil {
			t.Errorf("refused endpoint %q", ok)
		}
	}
	for _, bad := range []string{"198.51.100.7", "198.51.100.7:0", "[2001:db8::1]:51820", "host:51820", "198.51.100.7:51820 x"} {
		if ValidEndpoint(bad) == nil {
			t.Errorf("accepted endpoint %q", bad)
		}
	}
	for _, ok := range []string{"192.168.50.0/24", "10.0.0.0/8", "172.16.0.0/12"} {
		if ValidRoute(ok) != nil {
			t.Errorf("refused route %q", ok)
		}
	}
	for _, bad := range []string{"0.0.0.0/0", "10.0.0.0/7", "192.168.50.1/24", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "fd00::/64", "lan"} {
		if ValidRoute(bad) == nil {
			t.Errorf("accepted route %q", bad)
		}
	}
}

// An endpoint is where other nodes send to: not an address nothing can be reached at (unspecified,
// link-local, where cloud metadata services live, multicast, reserved, broadcast). Loopback is
// accepted, because `join-here` on a machine whose host name is 127.0.0.1 passes one and an old
// node may have reported one, but it is not reachable by other machines.
func TestEndpointsMustBeAddressesNodesCanBeReachedAt(t *testing.T) {
	// 240.0.0.0/4 is reserved, but Linux takes addresses there and some private networks use them.
	for _, ok := range []string{"198.51.100.7:51820", "10.0.0.1:1", "192.168.1.20:51820", "100.96.0.9:51820", "100.64.0.1:51820", "127.0.0.1:51820", "127.0.1.1:51820", "240.0.0.1:51820"} {
		if err := ValidEndpoint(ok); err != nil {
			t.Errorf("refused endpoint %q: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"0.0.0.0:51820", "0.1.2.3:51820", "169.254.169.254:80", "169.254.0.1:51820", "224.0.0.1:51820", "239.255.255.250:1900",
		"255.255.255.255:51820", "[::ffff:169.254.169.254]:51820",
	} {
		if ValidEndpoint(bad) == nil {
			t.Errorf("accepted endpoint %q", bad)
		}
	}
	for text, want := range map[string]bool{
		"198.51.100.7:51820": true, "192.168.1.20:51820": true, "100.64.0.1:51820": true,
		"127.0.0.1:51820": false, "127.0.1.1:51820": false, "169.254.169.254:80": false, "0.0.0.0:1": false, "198.51.100.7": false, "": false,
	} {
		if got := ReachableEndpoint(text); got != want {
			t.Errorf("ReachableEndpoint(%q) = %v, want %v", text, got, want)
		}
	}
	// The same rule applies where the coordinator takes a node's report, and where an agent takes a
	// peer's endpoint from its configuration.
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	join := JoinRequest{Protocol: Version, Token: "t", Name: "web", PublicKey: key, ListenPort: DefaultPort, Endpoints: []string{"198.51.100.7:51820", "169.254.169.254:51820"}}
	if join.Check() == nil {
		t.Error("a join that offers a link-local endpoint was accepted")
	}
	sync := SyncRequest{Protocol: Version, ListenPort: DefaultPort, Endpoints: []string{"0.0.0.0:51820"}}
	if sync.Check() == nil {
		t.Error("a sync that offers the unspecified address was accepted")
	}
	c := Config{Name: "web", MeshCIDR: "100.96.0.0/16", Address: "100.96.0.5/16", ListenPort: DefaultPort,
		Peers: []Peer{{Name: "db", PublicKey: key, Endpoint: "169.254.169.254:51820", AllowedIPs: []string{"100.96.0.6/32"}}}}
	if c.Validate() == nil {
		t.Error("a configuration with a link-local peer endpoint was accepted")
	}
	c.Peers[0].Endpoint = "203.0.113.7:51820"
	if err := c.Validate(); err != nil {
		t.Errorf("a configuration with a public peer endpoint was refused: %v", err)
	}
}

func TestRequestsAreChecked(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	join := JoinRequest{Protocol: Version, Token: "t", Name: "web", PublicKey: key, ListenPort: DefaultPort,
		Endpoints: []string{"198.51.100.7:51820"}, AdvertiseRoutes: []string{"192.168.50.0/24"}, OS: "linux", Version: "0.6.0"}
	if err := join.Check(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*JoinRequest){
		func(r *JoinRequest) { r.Protocol = Version + 1 },
		func(r *JoinRequest) { r.Name = "Web Server" },
		func(r *JoinRequest) { r.PublicKey = "x" },
		func(r *JoinRequest) { r.ListenPort = 0 },
		func(r *JoinRequest) { r.Endpoints = []string{"evil"} },
		func(r *JoinRequest) { r.AdvertiseRoutes = []string{"0.0.0.0/0"} },
		func(r *JoinRequest) { r.Version = strings.Repeat("9", 65) },
	} {
		bad := join
		change(&bad)
		if bad.Check() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	sync := SyncRequest{Protocol: Version, ListenPort: DefaultPort, Peers: []PeerStatus{{PublicKey: key, Endpoint: "198.51.100.8:51820"}},
		Pings: []Ping{{Address: "100.96.0.2", RTTMicros: 300}}}
	if err := sync.Check(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SyncRequest){
		func(r *SyncRequest) { r.Peers[0].PublicKey = "x" },
		func(r *SyncRequest) { r.Pings = []Ping{{Address: "100.96.0.2", RTTMicros: -1}} },
		func(r *SyncRequest) { r.Error = strings.Repeat("e", 301) },
		func(r *SyncRequest) { r.Peers = make([]PeerStatus, MaxPeers+1) },
	} {
		bad := sync
		bad.Peers = append([]PeerStatus(nil), sync.Peers...)
		change(&bad)
		if bad.Check() == nil {
			t.Errorf("accepted a bad sync request")
		}
	}
}
