// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package meshagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ghostlink.local/outline-worker/internal/hostfw"
	"ghostlink.local/outline-worker/internal/meshproto"
)

// HostsPath is the file the agent keeps <node>.mesh names in.
var HostsPath = "/etc/hosts"

// Agent keeps this node in line with the coordinator.
type Agent struct {
	Dir     string
	Node    *Node
	Log     *log.Logger
	FW      *hostfw.Firewall
	Version string

	client    *Client
	dev       *device
	pinger    *pinger
	applied   *meshproto.Config
	endpoints map[string]string
	problem   string
	// fwProblem is set while the firewall can't be put back (see keepFirewall).
	fwProblem string
	fwChecked time.Time
	status    string
	// clean is set when nothing of this agent is on the host as far as this process knows: it took
	// everything down (teardown) and has set nothing up since. A process starts not knowing: an
	// earlier one may have been killed, with the interface, routes and rules in place.
	clean bool
}

// PeerView is a peer as `ghostlink-mesh status` shows it.
type PeerView struct {
	Name          string   `json:"name"`
	PublicKey     string   `json:"public_key"`
	Endpoint      string   `json:"endpoint,omitempty"`
	AllowedIPs    []string `json:"allowed_ips"`
	LastHandshake int64    `json:"last_handshake_unix"`
	RxBytes       uint64   `json:"rx_bytes"`
	TxBytes       uint64   `json:"tx_bytes"`
}

// Status is what the agent writes to status.json.
type Status struct {
	UpdatedUnix int64            `json:"updated_unix"`
	State       string           `json:"state"`
	Name        string           `json:"name"`
	Address     string           `json:"address"`
	Coordinator string           `json:"coordinator"`
	Relay       bool             `json:"relay"`
	Shared      []string         `json:"shared"`
	Routes      []string         `json:"routes"`
	Error       string           `json:"error,omitempty"`
	Peers       []PeerView       `json:"peers"`
	Pings       map[string]int64 `json:"pings_us"`
}

// ReadStatus reads the last status the running agent wrote.
func ReadStatus(dir string) (Status, error) {
	var s Status
	data, err := os.ReadFile(filepath.Join(dir, statusFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

// Serve runs until ctx ends or the node is removed. The last configuration stays in force while
// the coordinator is unreachable, so a coordinator outage does not cut existing links.
func (a *Agent) Serve(ctx context.Context) error {
	a.client = NewClient(a.Node.URL, a.Node.Pin)
	a.pinger = newPinger()
	a.endpoints = map[string]string{}
	stop := make(chan struct{})
	go a.pinger.run(stop)
	defer close(stop)
	defer a.teardown()
	backoff := time.Second
	quietUntil := time.Time{}
	for ctx.Err() == nil {
		// `ghostlink-mesh leave` removed the node from this machine (it may have been unable to
		// tell the coordinator): the agent stops instead of setting it up again.
		if a.leftTheMesh() {
			return nil
		}
		a.checkDevice()
		a.keepFirewall(ctx)
		started, request := time.Now(), a.report()
		resp, err := a.client.Sync(ctx, a.Node.Secret, request)
		// How long the coordinator took, not counting what the agent then does with the answer
		// (taking down what an earlier run left can take over a second).
		took := time.Since(started)
		if ctx.Err() != nil {
			return nil // stopping: the deferred teardown cleans up
		}
		// The call may have waited for seconds, and `leave` may have finished meanwhile: what the
		// coordinator answered is then not set up on a machine that just left (nor the status
		// file, nor the identity, written again).
		if a.leftTheMesh() {
			return nil
		}
		switch {
		case errors.Is(err, ErrRemoved):
			a.Log.Printf("this node was removed from the mesh; cleaning up")
			a.teardown()
			a.restoreForwarding()
			Forget(a.Dir)
			return nil
		case err != nil:
			if time.Now().After(quietUntil) {
				a.Log.Printf("%v (retrying; the current links keep working)", err)
				quietUntil = time.Now().Add(5 * time.Minute)
			}
			a.status = "coordinator unreachable"
			a.writeStatus()
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff, quietUntil = time.Second, time.Time{}
		a.status = resp.Status
		switch resp.Status {
		case meshproto.StatusActive:
			if resp.Config != nil && (a.applied == nil || resp.Config.Generation != a.applied.Generation) {
				if err := a.apply(resp.Config); err != nil {
					a.problem = err.Error()
					a.Log.Printf("could not apply the mesh configuration: %v", err)
					a.writeStatus()
					sleep(ctx, 5*time.Second)
					continue
				}
				a.Log.Printf("applied configuration: %s, %d peers", resp.Config.Address, len(resp.Config.Peers))
			}
		case meshproto.StatusPending, meshproto.StatusDisabled:
			// Take everything down, also at the first answer of a process that has set nothing up:
			// an agent that was killed (or crashed, or lost the power) leaves its interface, routes,
			// rules and /etc/hosts block behind, and its successor would otherwise report the node
			// as waiting or disabled while the mesh is still open to it. Done once; teardown is
			// harmless when there is nothing to remove.
			if !a.clean || a.applied != nil || a.dev != nil {
				if a.applied != nil || a.dev != nil {
					a.Log.Printf("node is %s; links are down until the administrator allows it", resp.Status)
				}
				a.teardown()
			}
			a.problem = ""
		case meshproto.StatusRemoved:
			a.teardown()
			a.restoreForwarding()
			Forget(a.Dir)
			return nil
		}
		a.writeStatus()
		if resp.Status != meshproto.StatusActive && request.Applied == 0 && took < time.Second {
			// With nothing set up to take down, an answer this quick comes from an older
			// coordinator, which answers at once while a node waits for approval or is disabled;
			// asking again right away would turn into a busy loop.
			sleep(ctx, 10*time.Second)
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// leftTheMesh reports that `ghostlink-mesh leave` removed this node from the machine, and says so.
func (a *Agent) leftTheMesh() bool {
	if !left(a.Dir) {
		return false
	}
	a.Log.Printf("this node left the mesh; stopping")
	return true
}

// problems is what is wrong on this node, as reported to the coordinator.
func (a *Agent) problems() string {
	switch {
	case a.fwProblem == "":
		return a.problem
	case a.problem == "":
		return a.fwProblem
	}
	return a.problem + "; " + a.fwProblem
}

func (a *Agent) report() meshproto.SyncRequest {
	r := meshproto.SyncRequest{
		Protocol: meshproto.Version, Version: a.Version, ListenPort: a.Node.ListenPort,
		Endpoints: LocalEndpoints(a.Node.Endpoint, a.Node.ListenPort, a.Node.Interface), AdvertiseRoutes: a.Node.AdvertiseRoutes,
		Error: a.problems(), Peers: []meshproto.PeerStatus{}, Pings: []meshproto.Ping{},
	}
	if len(r.Error) > 300 {
		r.Error = r.Error[:300]
	}
	if r.AdvertiseRoutes == nil {
		r.AdvertiseRoutes = []string{}
	}
	if a.applied != nil {
		r.Applied = a.applied.Generation
	}
	if a.dev != nil {
		if peers, err := a.dev.peers(); err == nil {
			for _, p := range peers {
				s := meshproto.PeerStatus{PublicKey: p.PublicKey.String(), RxBytes: uint64(p.ReceiveBytes), TxBytes: uint64(p.TransmitBytes)}
				if !p.LastHandshakeTime.IsZero() {
					s.LastHandshake = p.LastHandshakeTime.Unix()
				}
				if p.Endpoint != nil {
					s.Endpoint = p.Endpoint.String()
				}
				r.Peers = append(r.Peers, s)
			}
		}
	}
	for address, rtt := range a.pinger.snapshot() {
		r.Pings = append(r.Pings, meshproto.Ping{Address: address, RTTMicros: rtt})
	}
	return r
}

// checkDevice notices an interface deleted under the running agent (ip link del, a reloaded
// wireguard module). Forgetting what was applied makes the coordinator answer at once, and the
// configuration then creates the interface again.
func (a *Agent) checkDevice() {
	if a.dev != nil && a.dev.gone() {
		a.Log.Printf("%s disappeared; creating it again", a.Node.Interface)
		a.dev.close()
		a.dev, a.applied = nil, nil
	}
}

// rules renders c for this node. Without the node's own consent to forwarding it relays nothing
// and shares no subnets, even where IPv4 forwarding is already on (Docker hosts, for example).
func (n *Node) rules(c *meshproto.Config) map[string]string {
	if !n.Forwarding {
		local := *c
		local.Relay, local.Shared, local.Masquerade = false, nil, false
		c = &local
	}
	return Rules(c, n.Interface)
}

// firewall installs the chains for c, then the jumps to them.
func (a *Agent) firewall(ctx context.Context, c *meshproto.Config) error {
	if err := a.FW.Restore(ctx, a.Node.rules(c)); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	if err := a.FW.EnsureJumps(ctx, Jumps(a.Node.Interface)); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	return nil
}

// keepFirewall puts the firewall back when something on the host removed it (a firewall reload,
// netfilter-persistent, iptables -F), and adds the jumps in Docker's DOCKER-USER once Docker has
// made that chain (see hostfw). While a configuration is applied it checks the jumps every 30
// seconds; what removes them removes the chains too, so both are restored.
func (a *Agent) keepFirewall(ctx context.Context) {
	if a.applied == nil || time.Since(a.fwChecked) < 30*time.Second {
		return
	}
	a.fwChecked = time.Now()
	if a.FW.HasJumps(ctx, Jumps(a.Node.Interface)) {
		a.fwProblem = ""
		return
	}
	a.Log.Printf("the mesh firewall rules are missing on this machine (a firewall reload, or Docker started); restoring them")
	if err := a.firewall(ctx, a.applied); err != nil {
		a.fwProblem = "could not restore the " + err.Error()
		a.Log.Printf("%s", a.fwProblem)
		return
	}
	a.fwProblem = ""
}

// apply brings the firewall, interface, routes and hosts block in line with c.
func (a *Agent) apply(c *meshproto.Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refused configuration: %w", err)
	}
	// From here the host is no longer as the agent found it, even if what follows fails halfway.
	a.clean = false
	// The firewall goes in first, so the interface never carries traffic the rules would refuse,
	// and a firewall error leaves the node off the mesh instead of open to it.
	if err := a.firewall(context.Background(), c); err != nil {
		return err
	}
	a.fwChecked, a.fwProblem = time.Now(), ""
	if a.dev == nil {
		dev, err := openDevice(a.Node.Interface)
		if err != nil {
			return err
		}
		a.dev, a.endpoints = dev, map[string]string{}
	}
	current, err := a.dev.peers()
	if err != nil {
		return err
	}
	peers, unreachable := usablePeers(c)
	changes, endpoints, err := PeerChanges(current, peers, a.endpoints)
	if err != nil {
		return err
	}
	if err := a.dev.configure(a.Node.PrivateKey, c.ListenPort, changes); err != nil {
		return fmt.Errorf("configure WireGuard: %w", err)
	}
	a.endpoints = endpoints
	if err := a.dev.setAddress(c.Address); err != nil {
		return fmt.Errorf("set address: %w", err)
	}
	skipped, err := a.dev.setRoutes(c.Routes, outsideAddresses(a.client, peers))
	if err != nil {
		return err
	}
	var problems []string
	if len(skipped) > 0 {
		problems = append(problems, "not routing "+strings.Join(skipped, ", ")+" into the mesh: this machine is on that network itself, already routes it, or reaches a peer or the coordinator through it")
	}
	if len(unreachable) > 0 {
		problems = append(problems, "ignored the endpoint of "+strings.Join(unreachable, ", ")+": an address on loopback or inside the mesh network leads to no other machine")
	}
	if c.Relay || len(c.Shared) > 0 {
		if !a.Node.Forwarding {
			problems = append(problems, "the administrator made this node a relay or shared its subnets, but forwarding is not allowed on it; run on the node: sudo ghostlink-mesh allow-forwarding")
		} else {
			if a.Node.ForwardingBefore == "" {
				// Kept from the first time this node forwards: after a reboot, ip_forward can read
				// 0 only because Docker or libvirt have not started yet.
				if before := readForwarding(); before != "" {
					a.Node.ForwardingBefore = before
					_ = a.Node.saveIfJoined(a.Dir)
				}
			}
			if readForwarding() != "1" {
				if err := writeForwarding("1"); err != nil {
					problems = append(problems, "could not turn on IPv4 forwarding: "+err.Error())
				}
			}
		}
	}
	if a.Node.Hosts {
		if err := writeHosts(c.Hosts); err != nil {
			problems = append(problems, hostsProblem(err))
		}
	}
	var targets []string
	own, _ := netip.ParsePrefix(c.Address)
	for _, h := range c.Hosts {
		if h.Address != own.Addr().String() {
			targets = append(targets, h.Address)
		}
	}
	a.pinger.setTargets(targets)
	a.applied, a.problem = c, strings.Join(problems, "; ")
	return nil
}

// outsideAddresses are the addresses that must stay reachable outside the mesh: the coordinator's
// and the peers' endpoints. A shared subnet that carries one of them is not routed into the mesh
// (see meshRoutes): the endpoint of a peer would loop through the tunnel, and the coordinator,
// which is how the agent hears of changes, would be out of reach.
func outsideAddresses(client *Client, peers []meshproto.Peer) []netip.Addr {
	out := client.Addresses()
	for _, p := range peers {
		if ap, err := netip.ParseAddrPort(p.Endpoint); err == nil {
			out = append(out, ap.Addr())
		}
	}
	return out
}

// hostsProblem explains a failed /etc/hosts update.
func hostsProblem(err error) string {
	if errors.Is(err, syscall.EROFS) {
		// The service sees /etc/hosts through a bind mount of the file, which ends when another
		// program replaces the file (sed -i, Ansible, an editor saving a new copy).
		return HostsPath + " was replaced by another program, so the <node>.mesh names are not updated until the agent restarts: sudo systemctl restart ghostlink-mesh"
	}
	return "could not update " + HostsPath + ": " + err.Error()
}

// writeHosts updates the managed block in place (the file may be a bind mount).
func writeHosts(entries []meshproto.Host) error {
	existing, err := os.ReadFile(HostsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(entries) == 0 && !strings.Contains(string(existing), hostsBegin) {
		// Nothing of the agent's to remove: the file is left exactly as it is (it may not exist,
		// or be a bind mount that cannot be written). The agent takes its names down at every
		// start of a node that is waiting or disabled, in case an earlier run left them.
		return nil
	}
	next := RenderHosts(string(existing), entries)
	if next == string(existing) {
		return nil
	}
	return os.WriteFile(HostsPath, []byte(next), 0o644)
}

// teardown removes everything the agent put on the host, except forwarding (restored on leave).
func (a *Agent) teardown() {
	if a.dev != nil {
		a.dev.close()
		a.dev = nil
	}
	removeInterface(a.Node.Interface)
	a.FW.Remove(context.Background(), Jumps(a.Node.Interface), Chains())
	if a.Node.Hosts {
		if err := writeHosts(nil); err != nil && a.Log != nil {
			a.Log.Printf("could not remove the <node>.mesh names from %s: %v", HostsPath, err)
		}
	}
	a.applied, a.fwProblem, a.clean = nil, "", true
	if a.pinger != nil {
		a.pinger.setTargets(nil)
	}
}

// restoreForwarding sets IPv4 forwarding back to what it was before this node first forwarded.
// It stays on when software that may rely on it appeared on the machine since.
func (a *Agent) restoreForwarding() {
	if a.Node.ForwardingBefore == "" || a.Node.ForwardingBefore == "1" {
		return
	}
	if users := forwardingUsers(interfaceNames()); len(users) > 0 {
		if a.Log != nil {
			a.Log.Printf("IPv4 forwarding stays on: %s on this machine may need it", strings.Join(users, ", "))
		}
		return
	}
	_ = writeForwarding(a.Node.ForwardingBefore)
}

func (a *Agent) writeStatus() {
	s := Status{UpdatedUnix: time.Now().Unix(), State: a.status, Name: a.Node.Name, Address: a.Node.Address,
		Coordinator: a.Node.URL, Error: a.problems(), Peers: []PeerView{}, Pings: a.pinger.snapshot(), Shared: []string{}, Routes: []string{}}
	if a.applied != nil {
		s.Relay, s.Shared, s.Routes = a.applied.Relay, a.applied.Shared, a.applied.Routes
		names := map[string]meshproto.Peer{}
		for _, p := range a.applied.Peers {
			names[p.PublicKey] = p
		}
		if a.dev != nil {
			if peers, err := a.dev.peers(); err == nil {
				for _, p := range peers {
					v := PeerView{Name: names[p.PublicKey.String()].Name, PublicKey: p.PublicKey.String(), AllowedIPs: names[p.PublicKey.String()].AllowedIPs,
						RxBytes: uint64(p.ReceiveBytes), TxBytes: uint64(p.TransmitBytes)}
					if !p.LastHandshakeTime.IsZero() {
						v.LastHandshake = p.LastHandshakeTime.Unix()
					}
					if p.Endpoint != nil {
						v.Endpoint = p.Endpoint.String()
					}
					s.Peers = append(s.Peers, v)
				}
			}
		}
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	_ = writeAtomic(filepath.Join(a.Dir, statusFile), data, 0o644)
}

// Leave removes this node from the mesh and from the host. A running agent is stopped first: it
// is asked to stop with SIGTERM (when it holds the agent lock, see stopAgent), takes down what it
// set up and ends with status 0, so systemd does not start it again; the identity is deleted only
// after that, while Leave itself holds the lock, so no agent starts in between. When the agent
// does not stop in time (or there is no lock to be had), Leave goes on without it, as it did
// before the lock existed: a running agent sees that the identity is gone (before each sync and
// after it, whatever the coordinator answered) and stops without setting anything up or saving
// anything again. The two clean-ups may then run at once; each is safe to repeat.
func Leave(dir string, fw *hostfw.Firewall) error {
	return leave(dir, fw, osLocks, agentStopWait, log.New(os.Stderr, "ghostlink-mesh: ", 0))
}

func leave(dir string, fw *hostfw.Firewall, sys osLock, wait time.Duration, logger *log.Logger) error {
	if _, err := LoadNode(dir); err != nil {
		return err
	}
	lock := stopAgent(sys, dir, wait, logger.Printf)
	defer lock.Release()
	// Read again: the agent may have saved something (the forwarding setting from before) while it
	// ended, or the coordinator may have dropped the node and the agent cleaned up by itself.
	node, err := LoadNode(dir)
	if errors.Is(err, ErrNotJoined) {
		return nil
	}
	if err != nil {
		return err
	}
	remoteErr := NewClient(node.URL, node.Pin).Leave(node.Secret)
	if errors.Is(remoteErr, ErrRemoved) {
		remoteErr = nil
	}
	a := &Agent{Dir: dir, Node: node, FW: fw, Log: logger}
	a.teardown()
	a.restoreForwarding()
	Forget(dir)
	if remoteErr != nil {
		return fmt.Errorf("removed here, but the coordinator could not be told (%v); remove %s in the Ghostlink app", remoteErr, node.Name)
	}
	return nil
}
