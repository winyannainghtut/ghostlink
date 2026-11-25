// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package meshagent

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/meshproto"
)

// A machine with containers or VMs has bridges whose addresses are private to them. The agent
// must not offer those to other nodes as places to reach this machine, whichever software made
// them: the interfaces forwardingUsers knows must all be left out, not only some.
func TestVirtualInterfacesAreNotOffered(t *testing.T) {
	for _, name := range []string{
		"docker0", "br-3f2a19c0", "veth1a2b3c", "virbr0", "cni0", "cni-podman0", "flannel.1", "kube-ipvs0", "lxcbr0", "lxc_health",
		"tun0", "wg0", "tailscale0", "zt5u4abcde", "glmesh0", "glipsec0",
		"podman0", "podman1", "lxdbr0", "incusbr0", "cali1a2b3c4d5e6", "cilium_host", "cilium_net", "cilium_vxlan",
	} {
		if !virtualInterface(name) {
			t.Errorf("%s would be offered as an address of this machine", name)
		}
	}
	for _, name := range []string{"eth0", "eth0.100", "ens3", "enp1s0", "eno1", "bond0", "team0", "br0", "vmbr0", "wlan0", "wlp2s0"} {
		if virtualInterface(name) {
			t.Errorf("%s holds the machine's own address, but is left out", name)
		}
	}
	// What makes the interfaces of software that forwards is no reason to offer them either.
	for _, k := range forwarders {
		if !virtualInterface(k.prefix + "0") {
			t.Errorf("%s (%s) is a forwarder but its interface would be offered", k.prefix, k.name)
		}
	}
}

// An endpoint only this machine can reach (a loopback address, which `join-here` passes on a host
// whose name resolves to 127.0.0.1) is not offered to the other nodes, while a public one is.
func TestLocalEndpointsOfferOnlyReachableAddresses(t *testing.T) {
	if got := LocalEndpoints("198.51.100.7:51820", 51820, "glmesh0"); len(got) == 0 || got[0] != "198.51.100.7:51820" {
		t.Fatalf("the configured public endpoint is not first: %v", got)
	}
	for _, loopback := range []string{"127.0.0.1:51820", "127.0.1.1:51820"} {
		if got := LocalEndpoints(loopback, 51820, "glmesh0"); slices.Contains(got, loopback) {
			t.Errorf("%s was offered to other nodes", loopback)
		}
	}
	for _, offered := range LocalEndpoints("", 51820, "glmesh0") {
		if err := meshproto.ValidEndpoint(offered); err != nil || !meshproto.ReachableEndpoint(offered) {
			t.Errorf("this machine offers %s: %v", offered, err)
		}
	}
}

// A peer whose endpoint no other machine can be reached at (loopback, or inside the mesh network,
// where WireGuard would send into its own tunnel) is kept without an endpoint; the rest is as sent,
// and the configuration is not changed.
func TestUnusablePeerEndpointsAreLeftOut(t *testing.T) {
	c := &meshproto.Config{MeshCIDR: "100.96.0.0/16", Address: "100.96.0.5/16", Peers: []meshproto.Peer{
		{Name: "loop", Endpoint: "127.0.0.1:51820", AllowedIPs: []string{"100.96.0.2/32"}},
		{Name: "inside", Endpoint: "100.96.0.9:51820", AllowedIPs: []string{"100.96.0.3/32"}},
		{Name: "public", Endpoint: "203.0.113.7:51820", AllowedIPs: []string{"100.96.0.4/32"}},
		{Name: "private", Endpoint: "192.168.1.20:51820", AllowedIPs: []string{"100.96.0.6/32"}},
		{Name: "nat", AllowedIPs: []string{"100.96.0.7/32"}},
	}}
	peers, changed := usablePeers(c)
	if !slices.Equal(changed, []string{"loop", "inside"}) {
		t.Fatalf("changed %v", changed)
	}
	var endpoints []string
	for _, p := range peers {
		endpoints = append(endpoints, p.Endpoint)
		if len(p.AllowedIPs) != 1 {
			t.Errorf("%s lost its addresses", p.Name)
		}
	}
	if want := []string{"", "", "203.0.113.7:51820", "192.168.1.20:51820", ""}; !slices.Equal(endpoints, want) {
		t.Fatalf("endpoints %v, want %v", endpoints, want)
	}
	if c.Peers[0].Endpoint != "127.0.0.1:51820" || c.Peers[1].Endpoint != "100.96.0.9:51820" {
		t.Fatal("the configuration itself was changed")
	}
	// Without peers there is nothing to change.
	if peers, changed := usablePeers(&meshproto.Config{MeshCIDR: "100.96.0.0/16"}); len(peers) != 0 || len(changed) != 0 {
		t.Fatalf("%v %v", peers, changed)
	}
}

// The coordinator is how the agent hears of changes. A subnet another node shares must not be
// routed into the mesh if it carries the coordinator's address (the agent would cut itself off),
// any more than one that carries a peer's endpoint.
func TestRoutesNeverCarryTheCoordinator(t *testing.T) {
	routes := []string{"203.0.113.0/24", "198.51.100.0/24", "192.168.50.0/24"}
	peers := []meshproto.Peer{{Name: "web", Endpoint: "198.51.100.7:51820"}, {Name: "behind-nat"}}

	byAddress := NewClient("https://203.0.113.10:14810", strings.Repeat("A", 64))
	outside := outsideAddresses(byAddress, peers)
	for _, want := range []string{"203.0.113.10", "198.51.100.7"} {
		if !slices.Contains(outside, netip.MustParseAddr(want)) {
			t.Fatalf("%s is not kept outside the mesh: %v", want, outside)
		}
	}
	keep, skip := meshRoutes(routes, nil, nil, outside)
	if !slices.Equal(keep, []string{"192.168.50.0/24"}) || !slices.Equal(skip, []string{"203.0.113.0/24", "198.51.100.0/24"}) {
		t.Fatalf("keep %v, skip %v", keep, skip)
	}
	// Without the coordinator, as the agent once did, only the peers' subnet is left out.
	keep, _ = meshRoutes(routes, nil, nil, outsideAddresses(nil, peers))
	if !slices.Contains(keep, "203.0.113.0/24") {
		t.Fatalf("a client without addresses protects the coordinator: %v", keep)
	}
	// A coordinator already reached by a more specific route (its own /32 on the LAN) stays reachable.
	keep, _ = meshRoutes([]string{"203.0.113.0/24"}, nil, []netip.Prefix{netip.MustParsePrefix("203.0.113.10/32")}, outside)
	if !slices.Equal(keep, []string{"203.0.113.0/24"}) {
		t.Fatalf("a covered coordinator blocked its subnet: %v", keep)
	}
}

// When the URL names a host, the address is the one the last connection went to.
func TestClientRemembersWhereTheCoordinatorWas(t *testing.T) {
	node, _ := fakeCoordinator(t, meshproto.StatusPending)
	named := NewClient(strings.Replace(node.URL, "127.0.0.1", "localhost", 1), node.Pin)
	if got := named.Addresses(); len(got) != 0 {
		t.Fatalf("addresses before any connection to a named host: %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := named.Sync(ctx, node.Secret, meshproto.SyncRequest{Protocol: meshproto.Version, ListenPort: 51820}); err != nil {
		t.Fatal(err)
	}
	if got := named.Addresses(); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("127.0.0.1")}) {
		t.Fatalf("after a connection: %v", got)
	}
	// An address in the URL is known at once, and not listed twice once connected.
	literal := NewClient(node.URL, node.Pin)
	if got := literal.Addresses(); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("127.0.0.1")}) {
		t.Fatalf("a URL with an address: %v", got)
	}
	if _, err := literal.Sync(ctx, node.Secret, meshproto.SyncRequest{Protocol: meshproto.Version, ListenPort: 51820}); err != nil {
		t.Fatal(err)
	}
	if got := literal.Addresses(); len(got) != 1 {
		t.Fatalf("listed twice: %v", got)
	}
	// IPv6 addresses are not routed by the agent, so they are not listed.
	if got := NewClient("https://[2001:db8::1]:14810", node.Pin).Addresses(); len(got) != 0 {
		t.Fatalf("IPv6: %v", got)
	}
}
