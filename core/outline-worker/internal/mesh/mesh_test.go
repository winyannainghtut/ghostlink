// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package mesh

import (
	"crypto/rand"
	"encoding/base64"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/meshproto"
)

func publicKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

type fixture struct {
	t   *testing.T
	n   *Network
	l   *Live
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, n: &Network{}, l: NewLive(), now: time.Unix(1_800_000_000, 0)}
	if err := f.n.Enable("", "", 20000); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) join(name string, o InviteOptions, endpoints ...string) *Node {
	f.t.Helper()
	_, secret, err := f.n.CreateToken(o, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	node, _, err := f.n.Join(&meshproto.JoinRequest{Protocol: meshproto.Version, Token: secret, Name: name,
		PublicKey: publicKey(f.t), ListenPort: 51820, Endpoints: endpoints, OS: "linux", Version: "test"}, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	return node
}

// sync records a report from node with its interface set up: remote is where the coordinator
// sees it from, and handshakes lists the peers it has a fresh handshake with.
func (f *fixture) sync(node *Node, remote string, handshakes ...*Node) {
	f.t.Helper()
	at := map[*Node]time.Time{}
	for _, peer := range handshakes {
		at[peer] = f.now.Add(-10 * time.Second)
	}
	f.report(node, remote, 1, at)
}

// report records a sync from node: applied is the generation it applied (0 right after the
// agent started), handshakes the last handshake with each peer it lists.
func (f *fixture) report(node *Node, remote string, applied uint64, handshakes map[*Node]time.Time) {
	f.t.Helper()
	r := &meshproto.SyncRequest{Protocol: meshproto.Version, ListenPort: 51820, Applied: applied}
	for peer, at := range handshakes {
		r.Peers = append(r.Peers, meshproto.PeerStatus{PublicKey: peer.PublicKey, LastHandshake: at.Unix(), RxBytes: 1000, TxBytes: 2000})
		r.Pings = append(r.Pings, meshproto.Ping{Address: peer.Address, RTTMicros: 4200})
	}
	f.l.Record(node.ID, netip.MustParseAddr(remote), r, f.now)
}

func (f *fixture) config(node *Node) *meshproto.Config {
	f.t.Helper()
	c, err := f.l.Config(f.n, node, f.now)
	if err != nil {
		f.t.Fatalf("config for %s: %v", node.Name, err)
	}
	return c
}

func peerNamed(c *meshproto.Config, name string) *meshproto.Peer {
	for i := range c.Peers {
		if c.Peers[i].Name == name {
			return &c.Peers[i]
		}
	}
	return nil
}

func TestInvitesAndJoining(t *testing.T) {
	f := newFixture(t)
	token, secret, err := f.n.CreateToken(InviteOptions{Tags: []string{"Web", "web"}}, f.now)
	if err != nil || token.UsesLeft != 1 || !slices.Equal(token.Tags, []string{"web"}) || strings.Contains(token.Hash, secret) {
		t.Fatalf("token: %+v %v", token, err)
	}
	request := func(name, secret string) *meshproto.JoinRequest {
		return &meshproto.JoinRequest{Protocol: meshproto.Version, Token: secret, Name: name, PublicKey: publicKey(t), ListenPort: 51820}
	}
	a, nodeSecret, err := f.n.Join(request("alpha", secret), f.now)
	if err != nil || a.Address != "100.96.0.1" || a.Status != meshproto.StatusActive || !slices.Equal(a.Tags, []string{"web"}) {
		t.Fatalf("join: %+v %v", a, err)
	}
	if _, _, err := f.n.Join(request("beta", secret), f.now); err != ErrInvite {
		t.Fatalf("a single-use invite worked twice: %v", err)
	}
	if f.n.Authenticate(a.ID+"."+nodeSecret) != a || f.n.Authenticate(a.ID+".wrong") != nil || f.n.Authenticate(nodeSecret) != nil {
		t.Fatal("node authentication")
	}
	_, expiring, _ := f.n.CreateToken(InviteOptions{Hours: 1}, f.now)
	if _, _, err := f.n.Join(request("beta", expiring), f.now.Add(2*time.Hour)); err != ErrInvite {
		t.Fatalf("an expired invite worked: %v", err)
	}
	_, approval, _ := f.n.CreateToken(InviteOptions{Approval: true, Uses: 2}, f.now)
	b, _, err := f.n.Join(request("beta", approval), f.now)
	if err != nil || b.Status != meshproto.StatusPending || b.Address != "100.96.0.2" {
		t.Fatalf("approval invite: %+v %v", b, err)
	}
	if _, _, err := f.n.Join(request("beta", approval), f.now); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, _, err := f.n.Join(request("Bad Name", approval), f.now); err == nil {
		t.Fatal("an invalid name joined")
	}
	if _, err := f.l.Config(f.n, b, f.now); err == nil {
		t.Fatal("a pending node got a configuration")
	}
	if f.n.Remove(a.ID); f.n.Node(a.ID) != nil {
		t.Fatal("remove")
	}
	if err := f.n.Enable("10.0.0.0/8", "", 20000); err == nil {
		t.Fatal("the network changed while it has nodes")
	}
}

// Public nodes connect directly; two nodes behind NAT go through the relay, and their ranges
// move into the relay peer without ever overlapping.
func TestDirectAndRelayedPaths(t *testing.T) {
	f := newFixture(t)
	relay := f.join("relay", InviteOptions{Relay: true}, "203.0.113.1:51820")
	public := f.join("public", InviteOptions{})
	natA := f.join("office-a", InviteOptions{}, "192.168.1.10:51820")
	natB := f.join("office-b", InviteOptions{}, "192.168.7.20:51820")
	f.sync(relay, "203.0.113.1")
	f.sync(public, "198.51.100.7")
	f.sync(natA, "198.51.100.50")  // behind a NAT router; its listen port is not reachable
	f.sync(natB, "198.51.100.200") // behind another one
	// Both offices are reachable at their observed public address only if the NAT is 1:1; the
	// coordinator tries direct first.
	f.l.Tick(f.n, f.now)
	if f.l.relayed(natA, natB) {
		t.Fatal("pair relayed before trying direct")
	}
	// No handshake within the grace period: relay.
	f.now = f.now.Add(directGrace + time.Second)
	f.sync(relay, "203.0.113.1", public, natA, natB)
	f.sync(public, "198.51.100.7", relay, natA, natB)
	f.sync(natA, "198.51.100.50", relay, public)
	f.sync(natB, "198.51.100.200", relay, public)
	if !f.l.Tick(f.n, f.now) || !f.l.relayed(natA, natB) || f.l.relayed(public, natA) {
		t.Fatal("the office pair should be relayed, the rest direct")
	}
	ca := f.config(natA)
	if !slices.Contains(peerNamed(ca, "relay").AllowedIPs, natB.Address+"/32") {
		t.Fatalf("office-a reaches office-b through the relay: %+v", ca.Peers)
	}
	// office-b stays a peer without addresses: a probe for when direct works again.
	if probe := peerNamed(ca, "office-b"); probe == nil || len(probe.AllowedIPs) != 0 || probe.Endpoint != "198.51.100.200:51820" || probe.Keepalive != keepalive {
		t.Fatalf("probe for office-b: %+v", probe)
	}
	if err := ca.Validate(); err != nil {
		t.Fatalf("a configuration with a probe: %v", err)
	}
	if peerNamed(ca, "public").Endpoint != "198.51.100.7:51820" || peerNamed(ca, "relay").Endpoint != "203.0.113.1:51820" {
		t.Fatalf("endpoints: %+v", ca.Peers)
	}
	if cr := f.config(relay); !cr.Relay || len(cr.Peers) != 3 || peerNamed(cr, "office-a").Keepalive != keepalive {
		t.Fatalf("relay config: %+v", cr)
	}
	topo := f.l.Topology(f.n, f.now)
	var officeLink LinkView
	for _, link := range topo.Links {
		if (link.A == natA.ID && link.B == natB.ID) || (link.A == natB.ID && link.B == natA.ID) {
			officeLink = link
		}
	}
	if officeLink.Path != "relay" || officeLink.Via != relay.ID {
		t.Fatalf("office link: %+v", officeLink)
	}
	// Traffic stays on the relay for as long as the probes get no handshake: no blind switch
	// back to a direct path that does not work.
	for i := 0; i < 400; i++ {
		f.now = f.now.Add(5 * time.Second)
		f.sync(natA, "198.51.100.50", relay, public)
		f.sync(natB, "198.51.100.200", relay, public)
		if f.l.Tick(f.n, f.now); !f.l.relayed(natA, natB) {
			t.Fatalf("the pair left the relay after %v without a direct handshake", time.Duration(i+1)*5*time.Second)
		}
	}
	// A handshake on the probe moves the pair back to direct.
	f.sync(natA, "198.51.100.50", relay, public, natB)
	if !f.l.Tick(f.n, f.now) || f.l.relayed(natA, natB) {
		t.Fatal("a direct handshake did not bring the pair back from the relay")
	}
	if p := peerNamed(f.config(natA), "office-b"); p == nil || !slices.Equal(p.AllowedIPs, []string{natB.Address + "/32"}) {
		t.Fatalf("office-b direct again: %+v", p)
	}
	// Nodes behind the same public address use their private addresses.
	f.sync(natB, "198.51.100.50")
	if e := f.l.endpoint(netip.MustParsePrefix(f.n.CIDR), natA, natB); e != "192.168.7.20:51820" {
		t.Fatalf("same-site endpoint: %s", e)
	}
}

// A node that comes back after a few minutes away keeps its direct links: its first sync, before
// its interface is set up again, reports no peers, and its peers' last handshakes with it are old
// by then. The grace period starts once it is back.
func TestReturningNodeKeepsDirectLinks(t *testing.T) {
	f := newFixture(t)
	relay := f.join("relay", InviteOptions{Relay: true}, "203.0.113.1:51820")
	a := f.join("alpha", InviteOptions{}, "198.51.100.7:51820")
	b := f.join("beta", InviteOptions{}, "198.51.100.8:51820")
	f.sync(relay, "203.0.113.1", a, b)
	f.sync(a, "198.51.100.7", relay, b)
	f.sync(b, "198.51.100.8", relay, a)
	f.l.Tick(f.n, f.now)
	lastWithBeta := f.now.Add(-10 * time.Second)
	// beta is away for 4 minutes; alpha keeps reporting its last handshake with beta.
	for i := 0; i < 48; i++ {
		f.now = f.now.Add(5 * time.Second)
		f.sync(relay, "203.0.113.1", a)
		f.report(a, "198.51.100.7", 1, map[*Node]time.Time{relay: f.now.Add(-10 * time.Second), b: lastWithBeta})
		f.l.Tick(f.n, f.now)
	}
	// beta's agent starts again: its first sync has applied nothing and lists no peers.
	f.report(b, "198.51.100.8", 0, nil)
	f.now = f.now.Add(200 * time.Millisecond)
	if f.l.Tick(f.n, f.now); f.l.relayed(a, b) {
		t.Fatal("the pair went to the relay before beta had set up its interface")
	}
	f.now = f.now.Add(2 * time.Second)
	f.sync(b, "198.51.100.8", relay, a)
	f.sync(a, "198.51.100.7", relay, b)
	if f.l.Tick(f.n, f.now); f.l.relayed(a, b) {
		t.Fatal("the pair went to the relay although both report a fresh handshake")
	}
	if p := peerNamed(f.config(a), "beta"); p == nil || !slices.Equal(p.AllowedIPs, []string{b.Address + "/32"}) {
		t.Fatalf("alpha's peer beta: %+v", p)
	}
}

// skewedReport records a sync from node whose own clock is off by skew (negative: behind the
// coordinator's): the handshake times it reports are in that clock. handshakes are given as they
// happened, by the coordinator's clock.
func (f *fixture) skewedReport(node *Node, remote string, skew time.Duration, handshakes map[*Node]time.Time) {
	f.t.Helper()
	shifted := map[*Node]time.Time{}
	for peer, at := range handshakes {
		shifted[peer] = at.Add(skew)
	}
	f.report(node, remote, 1, shifted)
}

// skewFixture is a relay and two public nodes, alpha and beta, who can connect directly, with
// reports every 5 seconds from all three. Alpha and beta's clocks are off by skew; the relay's is
// right. Each handshake happens every 100 seconds while the link works.
type skewFixture struct {
	*fixture
	relay, a, b *Node
	skew        time.Duration
	last        time.Time // the last handshake of the pair, by the coordinator's clock
}

func newSkewFixture(t *testing.T, skew time.Duration) *skewFixture {
	f := newFixture(t)
	s := &skewFixture{fixture: f, skew: skew, last: f.now}
	s.relay = f.join("relay", InviteOptions{Relay: true}, "203.0.113.1:51820")
	s.a = f.join("alpha", InviteOptions{}, "198.51.100.7:51820")
	s.b = f.join("beta", InviteOptions{}, "198.51.100.8:51820")
	return s
}

// step moves the clock on 5 seconds and has every node report. With working, the pair of nodes
// handshakes every 100 seconds; without, it does not, and keeps reporting its last handshake.
func (s *skewFixture) step(working bool) {
	s.now = s.now.Add(5 * time.Second)
	if working && s.now.Sub(s.last) >= 100*time.Second {
		s.last = s.now
	}
	s.skewedReport(s.relay, "203.0.113.1", 0, map[*Node]time.Time{s.a: s.now.Add(-20 * time.Second), s.b: s.now.Add(-20 * time.Second)})
	s.skewedReport(s.a, "198.51.100.7", s.skew, map[*Node]time.Time{s.relay: s.now.Add(-20 * time.Second), s.b: s.last})
	s.skewedReport(s.b, "198.51.100.8", s.skew, map[*Node]time.Time{s.relay: s.now.Add(-20 * time.Second), s.a: s.last})
	s.l.Tick(s.n, s.now)
}

// An address no other node can send to is not offered as a node's endpoint: not a loopback one, which
// is the node's own machine, and not one inside the mesh, which would send the handshake into the
// tunnel it is meant to set up. The next candidate is offered instead, or none. The node that
// reported such an address is not refused for it, and the configurations of the other nodes are
// built all the same.
func TestEndpointsOtherNodesCannotSendToAreNotOffered(t *testing.T) {
	mesh := netip.MustParsePrefix("100.96.0.0/16")
	f := newFixture(t)
	viewer := f.join("viewer", InviteOptions{}, "192.168.1.10:51820")
	loopbackFirst := f.join("loopback-first", InviteOptions{}, "127.0.0.1:51820", "100.96.0.9:51820", "192.168.7.20:51820")
	onlyUnusable := f.join("only-unusable", InviteOptions{}, "127.0.0.1:51820", "100.96.0.9:51820")
	publicToo := f.join("public-too", InviteOptions{}, "127.0.0.1:51820", "100.96.0.10:51820", "203.0.113.40:51820")
	// All behind one public address, so that private addresses are what is offered.
	for _, node := range []*Node{viewer, loopbackFirst, onlyUnusable, publicToo} {
		f.sync(node, "198.51.100.50")
	}
	for name, c := range map[string]struct {
		target *Node
		want   string
	}{
		"the next candidate after a loopback and an in-mesh address": {loopbackFirst, "192.168.7.20:51820"},
		"the address the coordinator sees when none is left":         {onlyUnusable, "198.51.100.50:51820"},
		"a public address after a loopback and an in-mesh one":       {publicToo, "203.0.113.40:51820"},
	} {
		if got := f.l.endpoint(mesh, viewer, c.target); got != c.want {
			t.Errorf("%s: offered %q, want %q", name, got, c.want)
		}
	}

	// Nodes on one private network, with no public address to share: the same.
	lan := newFixture(t)
	a := lan.join("a", InviteOptions{}, "10.0.0.5:51820")
	b := lan.join("b", InviteOptions{}, "127.0.0.1:51820", "10.0.0.6:51820")
	c := lan.join("c", InviteOptions{}, "127.0.0.1:51820", "100.96.0.9:51820")
	for i, node := range []*Node{a, b, c} {
		lan.sync(node, "10.0.0."+string(rune('5'+i)))
	}
	if got := lan.l.endpoint(mesh, a, b); got != "10.0.0.6:51820" {
		t.Errorf("on one LAN, a loopback address first: offered %q, want 10.0.0.6:51820", got)
	}
	if got := lan.l.endpoint(mesh, a, c); got != "" {
		t.Errorf("on one LAN, only addresses nobody can send to: offered %q, want none", got)
	}

	// The address the coordinator sees a node at is not offered either when it is inside the mesh.
	seen := f.join("seen", InviteOptions{})
	f.sync(seen, "198.51.100.200")
	if got := f.l.endpoint(netip.MustParsePrefix("198.51.100.0/24"), viewer, seen); got != "" {
		t.Errorf("an address inside the mesh network, seen by the coordinator: offered %q, want none", got)
	}
	if got := f.l.endpoint(mesh, viewer, seen); got != "198.51.100.200:51820" {
		t.Errorf("the address the coordinator sees the node at: offered %q", got)
	}

	// A configuration is built for every node, whatever a node reported, and offers its peers the
	// addresses that can be sent to.
	config := f.config(viewer)
	for name, want := range map[string]string{"loopback-first": "192.168.7.20:51820", "only-unusable": "198.51.100.50:51820", "public-too": "203.0.113.40:51820", "seen": "198.51.100.200:51820"} {
		peer := peerNamed(config, name)
		if peer == nil || peer.Endpoint != want {
			t.Errorf("viewer's configuration, peer %s: %+v, want endpoint %q", name, peer, want)
		}
	}
	if err := config.Validate(); err != nil {
		t.Errorf("the configuration is refused: %v", err)
	}
	if config := f.config(onlyUnusable); len(config.Peers) != 4 {
		t.Errorf("the node that reported addresses nobody can send to gets %d peers, want 4", len(config.Peers))
	}
	// On one private network, a peer that has nothing to offer has no endpoint, and its configuration
	// is still built.
	if config := lan.config(a); peerNamed(config, "c") == nil || peerNamed(config, "c").Endpoint != "" || peerNamed(config, "b").Endpoint != "10.0.0.6:51820" {
		t.Errorf("LAN configuration of a: %+v", config.Peers)
	}
}

// A node whose clock is minutes behind the coordinator's keeps its direct links: how old a handshake
// is was worked out from the node's clock, so with a clock more than 3 minutes behind every
// handshake looked stale, and the pair was sent through the relay for good.
func TestDirectLinkSurvivesAClockThatIsBehind(t *testing.T) {
	for _, skew := range []time.Duration{-10 * time.Minute, -4 * time.Minute, -time.Hour} {
		s := newSkewFixture(t, skew)
		// The first reports of a node whose clock is off cannot say how old its handshakes are; the
		// pair may be sent through the relay until the first new handshake shows that it works.
		for i := 0; i < 4*12; i++ {
			s.step(true)
		}
		for i := 0; i < 20*12; i++ {
			s.step(true)
			if s.l.relayed(s.a, s.b) {
				t.Fatalf("clock %v off: the pair was relayed %v after it started, although it handshakes every 100 s", skew, s.now.Sub(time.Unix(1_800_000_000, 0)))
			}
		}
	}
}

// A direct link that stops working is sent through the relay when the clock of the nodes is minutes
// ahead too: a handshake time in the future was taken to be new for as long as the clock was ahead,
// so a broken link was never sent through the relay.
func TestBrokenDirectLinkIsRelayedWhateverTheClock(t *testing.T) {
	for _, skew := range []time.Duration{0, 10 * time.Minute, 5 * time.Hour, -10 * time.Minute} {
		s := newSkewFixture(t, skew)
		for i := 0; i < 6*12; i++ {
			s.step(true)
		}
		for s.now != s.last { // on until a handshake has just happened
			s.step(true)
		}
		if s.l.relayed(s.a, s.b) {
			t.Fatalf("clock %v off: relayed while it worked", skew)
		}
		// The link breaks. For nearly 3 minutes more, the last handshake is recent enough.
		broke := s.now
		for s.now.Sub(broke) < 2*time.Minute {
			s.step(false)
		}
		if s.l.relayed(s.a, s.b) {
			t.Fatalf("clock %v off: relayed %v after the last handshake, before it can be known to be broken", skew, s.now.Sub(s.last))
		}
		for s.now.Sub(broke) < 6*time.Minute {
			s.step(false)
		}
		if !s.l.relayed(s.a, s.b) {
			t.Fatalf("clock %v off: still direct %v after the last handshake", skew, s.now.Sub(s.last))
		}
	}
}

// A pair on the relay goes back to direct when a handshake happens on the probe peers, whatever the
// clock of the nodes says: with a clock behind, no handshake was ever later than the time the pair
// went to the relay, and it never came back.
func TestRelayedPairComesBackWhateverTheClock(t *testing.T) {
	for _, skew := range []time.Duration{-10 * time.Minute, 10 * time.Minute, 0} {
		s := newSkewFixture(t, skew)
		for i := 0; i < 4*12; i++ {
			s.step(true)
		}
		for i := 0; i < 8*12; i++ { // broken for long enough to be relayed
			s.step(false)
		}
		if !s.l.relayed(s.a, s.b) {
			t.Fatalf("clock %v off: the broken pair was not relayed", skew)
		}
		// Still broken: it stays on the relay.
		for i := 0; i < 5*12; i++ {
			s.step(false)
			if !s.l.relayed(s.a, s.b) {
				t.Fatalf("clock %v off: the pair left the relay without a new handshake", skew)
			}
		}
		// The link works again: a handshake every 100 s, and the pair comes back within a few of them.
		back := s.now
		for i := 0; i < 5*12 && s.l.relayed(s.a, s.b); i++ {
			s.step(true)
		}
		if s.l.relayed(s.a, s.b) {
			t.Fatalf("clock %v off: the pair did not come back %v after the link worked again", skew, s.now.Sub(back))
		}
	}
}

// A handshake is dated by the coordinator's clock: a new one by the report that shows it, a known one
// by the date it already had, and one seen for the first time by the node's clock, never as later
// than now.
func TestHandshakeSeenIsDatedByTheCoordinatorsClock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	peer := func(handshake time.Time) meshproto.PeerStatus {
		return meshproto.PeerStatus{PublicKey: "peer", LastHandshake: handshake.Unix()}
	}
	reportOf := func(p meshproto.PeerStatus, seen time.Time) *report {
		return &report{peers: map[string]meshproto.PeerStatus{p.PublicKey: p}, handshakes: map[string]time.Time{p.PublicKey: seen}}
	}
	for name, c := range map[string]struct {
		previous *report
		reported time.Time // what the node says, by its own clock
		want     time.Time
	}{
		"first time, a plausible time":     {nil, now.Add(-30 * time.Second), now.Add(-30 * time.Second)},
		"first time, in the future":        {nil, now.Add(10 * time.Minute), now},
		"first time, long ago":             {nil, now.Add(-10 * time.Minute), now.Add(-10 * time.Minute)},
		"the peer was not in the report":   {&report{peers: map[string]meshproto.PeerStatus{}, handshakes: map[string]time.Time{}}, now.Add(-time.Minute), now.Add(-time.Minute)},
		"the same handshake again":         {reportOf(peer(now.Add(-time.Hour)), now.Add(-90*time.Second)), now.Add(-time.Hour), now.Add(-90 * time.Second)},
		"a new one, the clock far behind":  {reportOf(peer(now.Add(-time.Hour)), now.Add(-90*time.Second)), now.Add(-time.Hour + 100*time.Second), now},
		"a new one, the clock far ahead":   {reportOf(peer(now.Add(time.Hour)), now.Add(-90*time.Second)), now.Add(time.Hour + 100*time.Second), now},
		"one that went back (a new clock)": {reportOf(peer(now.Add(time.Hour)), now.Add(-90*time.Second)), now.Add(-time.Hour), now},
		"the first one after none":         {&report{peers: map[string]meshproto.PeerStatus{"peer": {PublicKey: "peer"}}, handshakes: map[string]time.Time{}}, now.Add(-5 * time.Minute), now},
	} {
		if got := handshakeSeen(c.previous, peer(c.reported), now); !got.Equal(c.want) {
			t.Errorf("%s: dated %v, want %v", name, got.Sub(now), c.want.Sub(now))
		}
	}
}

// A report of a node that is not in the network any more is not kept: one that arrived while the
// node was removed would stay in memory for good.
func TestReportOfARemovedNodeIsNotKept(t *testing.T) {
	f := newFixture(t)
	a := f.join("alpha", InviteOptions{})
	b := f.join("beta", InviteOptions{})
	f.sync(a, "203.0.113.1")
	f.sync(b, "203.0.113.2")
	if got := f.l.Nodes(); got != 2 {
		t.Fatalf("%d nodes with a report, want 2", got)
	}
	f.n.Remove(b.ID)
	f.sync(b, "203.0.113.2") // a sync that was being answered when the node was removed
	f.now = f.now.Add(5 * time.Second)
	f.l.Tick(f.n, f.now)
	if got := f.l.Nodes(); got != 1 {
		t.Fatalf("%d nodes with a report after the removal of one, want 1", got)
	}
}

// A link's traffic counts each direction once, whether one node reports it or both do.
func TestLinkTrafficCountedOnce(t *testing.T) {
	f := newFixture(t)
	a := f.join("alpha", InviteOptions{}, "198.51.100.7:51820")
	b := f.join("beta", InviteOptions{}, "198.51.100.8:51820")
	record := func(node, peer *Node, remote string, rx, tx uint64) {
		f.l.Record(node.ID, netip.MustParseAddr(remote), &meshproto.SyncRequest{Protocol: meshproto.Version, ListenPort: 51820, Applied: 1,
			Peers: []meshproto.PeerStatus{{PublicKey: peer.PublicKey, LastHandshake: f.now.Unix(), RxBytes: rx, TxBytes: tx}}}, f.now)
	}
	record(a, b, "198.51.100.7", 0, 0)
	record(b, a, "198.51.100.8", 0, 0)
	// In 10 s alpha sends beta 20 000 bytes and beta sends alpha 10 000: 3000 B/s in all.
	f.now = f.now.Add(10 * time.Second)
	record(a, b, "198.51.100.7", 10_000, 20_000)
	record(b, a, "198.51.100.8", 20_000, 10_000)
	link := f.l.Topology(f.n, f.now).Links[0]
	if link.RxBps+link.TxBps != 3000 || (link.A == a.ID && (link.RxBps != 1000 || link.TxBps != 2000)) {
		t.Fatalf("both reporting: %+v", link)
	}
	// Only beta still reports: in 31 s it received 310 000 bytes from alpha and sent it 155 000.
	f.now = f.now.Add(OnlineFor + time.Second)
	record(b, a, "198.51.100.8", 330_000, 165_000)
	if link := f.l.Topology(f.n, f.now).Links[0]; link.RxBps+link.TxBps != 15_000 || (link.A == a.ID && link.TxBps != 10_000) {
		t.Fatalf("one reporting: %+v", link)
	}
}

func TestGenerationFollowsContent(t *testing.T) {
	f := newFixture(t)
	a := f.join("alpha", InviteOptions{})
	b := f.join("beta", InviteOptions{})
	f.sync(a, "203.0.113.1")
	f.sync(b, "203.0.113.2")
	first := f.config(a).Generation
	if again := f.config(a).Generation; again != first || first == 0 || first >= 1<<53 {
		t.Fatal("generation is not stable")
	}
	tags := []string{"db"}
	if err := f.n.Update(b, NodeChange{Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	if err := f.n.SetRules([]Rule{{From: "*", To: "tag:db", Protocol: "tcp", Ports: []string{"5432"}}}); err != nil {
		t.Fatal(err)
	}
	if f.config(a).Generation == first {
		t.Fatal("generation did not change with the rules")
	}
}

func TestRulesCompileToSources(t *testing.T) {
	f := newFixture(t)
	web := f.join("web", InviteOptions{Tags: []string{"web"}})
	db := f.join("db", InviteOptions{Tags: []string{"db"}})
	other := f.join("other", InviteOptions{})
	if c := f.config(db); !c.Firewall.Open {
		t.Fatal("a mesh without rules must be open")
	}
	for _, bad := range [][]Rule{
		{{From: "web", To: "tag:db", Protocol: "tcp"}},
		{{From: "*", To: "tag:db", Protocol: "icmp", Ports: []string{"1"}}},
		{{From: "*", To: "*", Protocol: "sctp"}},
		{{From: "*", To: "*", Protocol: "tcp", Ports: []string{"70000"}}},
	} {
		if err := f.n.SetRules(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if err := f.n.SetRules([]Rule{{From: "tag:web", To: "tag:db", Protocol: "tcp", Ports: []string{"5432"}}, {From: "node:db", To: "*", Protocol: "icmp"}}); err != nil {
		t.Fatal(err)
	}
	fw := f.config(db).Firewall
	if fw.Open || len(fw.Allow) != 1 || !slices.Equal(fw.Allow[0].Sources, []string{web.Address + "/32"}) ||
		!slices.Equal(fw.Allow[0].Destinations, []string{db.Address + "/32"}) || fw.Allow[0].Protocol != "tcp" {
		t.Fatalf("db firewall: %+v", fw)
	}
	if fw := f.config(other).Firewall; len(fw.Allow) != 1 || fw.Allow[0].Protocol != "icmp" || fw.Allow[0].Sources[0] != db.Address+"/32" {
		t.Fatalf("other firewall: %+v", fw)
	}
}

func TestSharedSubnets(t *testing.T) {
	f := newFixture(t)
	_, secret, _ := f.n.CreateToken(InviteOptions{}, f.now)
	router, _, err := f.n.Join(&meshproto.JoinRequest{Protocol: meshproto.Version, Token: secret, Name: "office", PublicKey: publicKey(t),
		ListenPort: 51820, AdvertiseRoutes: []string{"192.168.50.0/24", "10.20.0.0/16"}}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	other := f.join("cloud", InviteOptions{})
	if c := f.config(other); len(c.Routes) != 0 || len(peerNamed(c, "office").AllowedIPs) != 1 {
		t.Fatal("a subnet was shared before approval")
	}
	for _, bad := range [][]string{{"172.16.0.0/12"}, {"100.96.0.0/24"}, {"192.168.50.0/24", "192.168.50.0/25"}} {
		if err := f.n.Update(router, NodeChange{ApprovedRoutes: &bad}); err == nil {
			t.Fatalf("approved %v", bad)
		}
	}
	approve := []string{"192.168.50.0/24"}
	if err := f.n.Update(router, NodeChange{ApprovedRoutes: &approve}); err != nil {
		t.Fatal(err)
	}
	c := f.config(other)
	if !slices.Equal(c.Routes, approve) || !slices.Contains(peerNamed(c, "office").AllowedIPs, "192.168.50.0/24") {
		t.Fatalf("cloud config: %+v", c)
	}
	if r := f.config(router); !slices.Equal(r.Shared, approve) || !r.Masquerade {
		t.Fatalf("router config: %+v", r)
	}
	if hosts := c.Hosts; len(hosts) != 2 {
		t.Fatalf("hosts: %+v", hosts)
	}
}

func TestConfigValidationRefusesUnsafeInput(t *testing.T) {
	f := newFixture(t)
	a := f.join("alpha", InviteOptions{})
	f.join("beta", InviteOptions{})
	good := f.config(a)
	for name, mutate := range map[string]func(c *meshproto.Config){
		"peer claims own address": func(c *meshproto.Config) { c.Peers[0].AllowedIPs = append(c.Peers[0].AllowedIPs, a.Address+"/32") },
		"peer claims mesh range":  func(c *meshproto.Config) { c.Peers[0].AllowedIPs = []string{"100.96.0.0/24"} },
		"default route":           func(c *meshproto.Config) { c.Peers[0].AllowedIPs = append(c.Peers[0].AllowedIPs, "0.0.0.0/0") },
		"bad key":                 func(c *meshproto.Config) { c.Peers[0].PublicKey = "x" },
		"bad endpoint":            func(c *meshproto.Config) { c.Peers[0].Endpoint = "example.com:1" },
		"name injection":          func(c *meshproto.Config) { c.Peers[0].Name = "a\nb" },
		"bad protocol":            func(c *meshproto.Config) { c.Firewall.Allow = []meshproto.Allow{{Protocol: "gre"}} },
		"host outside mesh":       func(c *meshproto.Config) { c.Hosts = []meshproto.Host{{Name: "x", Address: "8.8.8.8"}} },
		"address outside mesh":    func(c *meshproto.Config) { c.Address = "10.0.0.1/16" },
	} {
		c := *good
		c.Peers = append([]meshproto.Peer{}, good.Peers...)
		c.Peers[0].AllowedIPs = append([]string{}, good.Peers[0].AllowedIPs...)
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	invite := meshproto.EncodeInvite(meshproto.Invite{URL: "https://203.0.113.1:20000", Pin: strings.Repeat("A", 64), Token: "abcdefghijklmnopqrstuv"})
	if got, err := meshproto.DecodeInvite("  " + invite + "\n"); err != nil || got.URL != "https://203.0.113.1:20000" {
		t.Fatalf("invite round trip: %+v %v", got, err)
	}
	if _, err := meshproto.DecodeInvite("glmesh1:not-base64!"); err == nil {
		t.Fatal("a damaged invite decoded")
	}
}
