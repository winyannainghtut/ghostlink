// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/mesh"
	"ghostlink.local/outline-worker/internal/meshproto"
)

// meshAgent talks to the coordinator the way ghostlink-mesh does: pinned TLS, JSON.
type meshAgent struct {
	t      *testing.T
	client *http.Client
	url    string
	secret string
	key    string
}

func newMeshAgent(t *testing.T, invite string) *meshAgent {
	t.Helper()
	inv, err := meshproto.DecodeInvite(invite)
	if err != nil {
		t.Fatal(err)
	}
	pin := inv.Pin
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, MinVersion: tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), pin) {
				t.Errorf("coordinator certificate does not match the invite's pin")
			}
			return nil
		},
	}}}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return &meshAgent{t: t, client: client, url: inv.URL, key: base64.StdEncoding.EncodeToString(key)}
}

func (a *meshAgent) post(path string, body any, out any) int {
	a.t.Helper()
	data, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPost, a.url+path, bytes.NewReader(data))
	if a.secret != "" {
		request.Header.Set("Authorization", "Bearer "+a.secret)
	}
	response, err := a.client.Do(request)
	if err != nil {
		a.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if out != nil && response.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			a.t.Fatalf("%s: %v: %s", path, err, raw)
		}
	}
	return response.StatusCode
}

func (a *meshAgent) join(token, name string, routes ...string) meshproto.JoinResponse {
	a.t.Helper()
	var out meshproto.JoinResponse
	if status := a.post("/v1/join", meshproto.JoinRequest{Protocol: meshproto.Version, Token: token, Name: name, PublicKey: a.key,
		ListenPort: 51820, Endpoints: []string{"203.0.113.9:51820"}, AdvertiseRoutes: routes, OS: "linux", Version: "test"}, &out); status != http.StatusOK {
		a.t.Fatalf("join %s: %d", name, status)
	}
	a.secret = out.Secret
	return out
}

func (a *meshAgent) sync(applied uint64, routes ...string) (meshproto.SyncResponse, int, time.Duration) {
	a.t.Helper()
	var out meshproto.SyncResponse
	start := time.Now()
	status := a.post("/v1/sync", meshproto.SyncRequest{Protocol: meshproto.Version, Version: "test", Applied: applied, ListenPort: 51820,
		Endpoints: []string{"203.0.113.9:51820"}, AdvertiseRoutes: routes, Peers: []meshproto.PeerStatus{}}, &out)
	return out, status, time.Since(start)
}

func TestMeshCoordinator(t *testing.T) {
	previousWait := meshWait
	meshWait = 1500 * time.Millisecond
	defer func() { meshWait = previousWait }()
	server := newTestServer(t, nil)
	meshServer := NewMeshServer(server.service)
	server.service.OnChange(func(previous, next *State) {
		if err := meshServer.Apply(); err != nil {
			t.Errorf("mesh apply: %v", err)
		}
	})
	t.Cleanup(meshServer.Close)

	if status := server.call(t, "POST", "/mesh/invites", map[string]any{}, nil); status != http.StatusBadRequest {
		t.Fatalf("invite before enabling: %d", status)
	}
	var info meshInfo
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, &info)
	if !info.Enabled || info.Port == 0 || info.CIDR != mesh.DefaultCIDR || info.URL == "" {
		t.Fatalf("mesh info: %+v", info)
	}
	server.service.Read(func(s *State) {
		if ports := s.Listening(); ports["mesh"] != "tcp/"+strings.TrimPrefix(info.URL, "https://127.0.0.1:") {
			t.Errorf("listening: %v", ports)
		}
		data, _ := s.EngineConfig("off")
		var config struct {
			Route struct {
				Rules []struct {
					Port   []uint16 `json:"port"`
					Action string   `json:"action"`
				} `json:"rules"`
			} `json:"route"`
		}
		_ = json.Unmarshal(data, &config)
		blocked := false
		for _, rule := range config.Route.Rules {
			blocked = blocked || (rule.Action == "reject" && slices.Contains(rule.Port, info.Port))
		}
		if !blocked {
			t.Error("VPN users are not kept off the mesh port")
		}
	})

	var created inviteCreated
	server.must(t, http.StatusCreated, "POST", "/mesh/invites", map[string]any{"tags": []string{"web"}, "local": true}, &created)
	if !strings.HasPrefix(created.Command, "curl -fsSL "+MeshScript+" | sudo bash -s -- --join 'glmesh1:") || strings.Contains(created.Command, "--relay") {
		t.Fatalf("join command: %s", created.Command)
	}
	// A relay invite's command gives the server's consent to forwarding.
	var relayInvite inviteCreated
	server.must(t, http.StatusCreated, "POST", "/mesh/invites", map[string]any{"relay": true}, &relayInvite)
	if !strings.HasSuffix(relayInvite.Command, "' --relay") {
		t.Fatalf("relay join command: %s", relayInvite.Command)
	}
	inv, _ := meshproto.DecodeInvite(created.Invite)
	pin := ""
	server.service.Read(func(s *State) { pin, _ = s.CertSHA256() })
	if inv.Pin != pin || !strings.HasPrefix(inv.URL, "https://127.0.0.1:") {
		t.Fatalf("invite: %+v", inv)
	}

	web := newMeshAgent(t, created.Invite)
	joined := web.join(inv.Token, "web-1", "10.20.0.0/24")
	if joined.Status != meshproto.StatusActive || joined.Address != "100.96.0.1" {
		t.Fatalf("join: %+v", joined)
	}
	// A used invite fails, and repeated failures are throttled.
	thief := newMeshAgent(t, created.Invite)
	if status := thief.post("/v1/join", meshproto.JoinRequest{Protocol: meshproto.Version, Token: inv.Token, Name: "thief", PublicKey: thief.key, ListenPort: 1}, nil); status != http.StatusForbidden {
		t.Fatalf("reused invite: %d", status)
	}

	first, status, took := web.sync(0)
	if status != http.StatusOK || first.Status != meshproto.StatusActive || first.Config == nil || took > time.Second {
		t.Fatalf("first sync: %d %+v after %v", status, first, took)
	}
	if first.Config.Name != "web-1" || first.Config.Address != "100.96.0.1/16" || !first.Config.Firewall.Open || len(first.Config.Peers) != 0 {
		t.Fatalf("first config: %+v", first.Config)
	}
	// Nothing new: the sync waits for the full period.
	same, _, took := web.sync(first.Config.Generation, "10.20.0.0/24")
	if same.Config.Generation != first.Config.Generation || took < meshWait-200*time.Millisecond {
		t.Fatalf("an unchanged sync returned after %v", took)
	}

	// A second node joining wakes the waiting sync of the first at once.
	var second inviteCreated
	server.must(t, http.StatusCreated, "POST", "/mesh/invites", map[string]any{"tags": []string{"db"}, "local": true}, &second)
	db := newMeshAgent(t, second.Invite)
	woke := make(chan time.Duration, 1)
	go func() {
		_, _, took := web.sync(first.Config.Generation, "10.20.0.0/24")
		woke <- took
	}()
	time.Sleep(200 * time.Millisecond)
	inv2, _ := meshproto.DecodeInvite(second.Invite)
	dbJoined := db.join(inv2.Token, "db-1")
	if took := <-woke; took > meshWait-300*time.Millisecond {
		t.Fatalf("the first node was not woken by the second joining (%v)", took)
	}
	withPeer, _, _ := web.sync(0, "10.20.0.0/24")
	if len(withPeer.Config.Peers) != 1 || withPeer.Config.Peers[0].Name != "db-1" || withPeer.Config.Peers[0].PublicKey != db.key {
		t.Fatalf("peers: %+v", withPeer.Config.Peers)
	}

	// Rules, routes and topology through the admin API.
	server.must(t, http.StatusOK, "PUT", "/mesh/rules", map[string]any{"rules": []mesh.Rule{{From: "tag:web", To: "tag:db", Protocol: "tcp", Ports: []string{"5432"}}}}, nil)
	if status := server.call(t, "PUT", "/mesh/rules", map[string]any{"rules": []mesh.Rule{{From: "web", To: "*", Protocol: "tcp"}}}, nil); status != http.StatusBadRequest {
		t.Fatalf("bad rule: %d", status)
	}
	dbConfig, _, _ := db.sync(0)
	if fw := dbConfig.Config.Firewall; fw.Open || len(fw.Allow) != 1 || fw.Allow[0].Sources[0] != "100.96.0.1/32" {
		t.Fatalf("db firewall: %+v", fw)
	}
	if status := server.call(t, "PATCH", "/mesh/nodes/"+joined.NodeID, map[string]any{"approved_routes": []string{"10.99.0.0/24"}}, nil); status != http.StatusBadRequest {
		t.Fatalf("approving a route the node does not offer: %d", status)
	}
	server.must(t, http.StatusOK, "PATCH", "/mesh/nodes/"+joined.NodeID, map[string]any{"approved_routes": []string{"10.20.0.0/24"}}, nil)
	dbConfig, _, _ = db.sync(0)
	if !slices.Equal(dbConfig.Config.Routes, []string{"10.20.0.0/24"}) {
		t.Fatalf("db routes: %+v", dbConfig.Config.Routes)
	}
	var topology mesh.Topology
	server.must(t, http.StatusOK, "GET", "/mesh/topology", nil, &topology)
	if len(topology.Nodes) != 2 || len(topology.Links) != 1 || !topology.Nodes[0].Online || topology.Nodes[0].SeenFrom != "127.0.0.1" {
		t.Fatalf("topology: %+v", topology)
	}

	// Removing a node ends its access; a bad secret is refused.
	server.must(t, http.StatusNoContent, "DELETE", "/mesh/nodes/"+dbJoined.NodeID, nil, nil)
	if _, status, _ := db.sync(0); status != http.StatusUnauthorized {
		t.Fatalf("a removed node synced: %d", status)
	}
	if after, _, _ := web.sync(0, "10.20.0.0/24"); len(after.Config.Peers) != 0 {
		t.Fatal("the removed node is still a peer")
	}
	// Turning the mesh off answers the waiting syncs with "disabled" before the port closes, so
	// their nodes take their links down; then the agent API stops.
	current, _, _ := web.sync(0, "10.20.0.0/24")
	told := make(chan string, 1)
	go func() {
		data, _ := json.Marshal(meshproto.SyncRequest{Protocol: meshproto.Version, Version: "test", Applied: current.Config.Generation,
			ListenPort: 51820, Endpoints: []string{"203.0.113.9:51820"}, AdvertiseRoutes: []string{"10.20.0.0/24"}, Peers: []meshproto.PeerStatus{}})
		request, _ := http.NewRequest(http.MethodPost, web.url+"/v1/sync", bytes.NewReader(data))
		request.Header.Set("Authorization", "Bearer "+web.secret)
		response, err := web.client.Do(request)
		if err != nil {
			told <- "no answer (" + err.Error() + ")"
			return
		}
		defer response.Body.Close()
		var out meshproto.SyncResponse
		_ = json.NewDecoder(response.Body).Decode(&out)
		told <- out.Status
	}()
	time.Sleep(300 * time.Millisecond)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": false}, nil)
	if status := <-told; status != meshproto.StatusDisabled {
		t.Fatalf("a waiting node was told %q when the mesh was turned off", status)
	}
	if _, err := web.client.Get(web.url + "/v1/sync"); err == nil {
		t.Fatal("the mesh port still answers after turning the mesh off")
	}
}

// A node waiting for approval waits for a change like an up-to-date node (answering it at once
// made it ask again at once, over and over), and hears at once when it is approved.
func TestMeshWaitingNodeLongPolls(t *testing.T) {
	previousWait := meshWait
	meshWait = 1500 * time.Millisecond
	defer func() { meshWait = previousWait }()
	server := newTestServer(t, nil)
	meshServer := NewMeshServer(server.service)
	server.service.OnChange(func(previous, next *State) {
		if err := meshServer.Apply(); err != nil {
			t.Errorf("mesh apply: %v", err)
		}
	})
	t.Cleanup(meshServer.Close)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, nil)
	var created inviteCreated
	server.must(t, http.StatusCreated, "POST", "/mesh/invites", map[string]any{"approval": true, "local": true}, &created)
	inv, _ := meshproto.DecodeInvite(created.Invite)
	agent := newMeshAgent(t, created.Invite)
	if joined := agent.join(inv.Token, "waiting-1"); joined.Status != meshproto.StatusPending {
		t.Fatalf("join: %+v", joined)
	}
	if out, _, took := agent.sync(0); out.Status != meshproto.StatusPending || took < meshWait-200*time.Millisecond {
		t.Fatalf("a waiting node was answered after %v: %+v", took, out)
	}
	type answer struct {
		out  meshproto.SyncResponse
		took time.Duration
	}
	woke := make(chan answer, 1)
	go func() {
		out, _, took := agent.sync(0)
		woke <- answer{out, took}
	}()
	time.Sleep(200 * time.Millisecond)
	var topology mesh.Topology
	server.must(t, http.StatusOK, "GET", "/mesh/topology", nil, &topology)
	server.must(t, http.StatusOK, "PATCH", "/mesh/nodes/"+topology.Nodes[0].ID, map[string]any{"status": "active"}, nil)
	if got := <-woke; got.out.Status != meshproto.StatusActive || got.out.Config == nil || got.took > meshWait-300*time.Millisecond {
		t.Fatalf("approval reached the waiting node after %v: %+v", got.took, got.out)
	}
}

// listensOn says whether something accepts TCP connections on the loopback port.
func listensOn(port uint16) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// A sync that is being answered while the mesh is turned off, and whose own change runs Apply (it
// reports something new, which is saved), does not make the turning off wait for the full 3 seconds
// the server gives the requests it answers. Before, Apply held its lock while it waited for the
// request to end, and the request's own Apply waited for the lock: each waited for the other until
// the 3 seconds were up.
func TestMeshApplyDoesNotHoldItsLockWhileARequestIsAnswered(t *testing.T) {
	previousWait, previousDrain := meshWait, meshDrain
	// The drain limit is long here so that a slow machine cannot end the sync with it: the old code
	// has its own, 3 seconds, and the test tells the two apart by how long the turning off takes.
	meshWait, meshDrain = 1500*time.Millisecond, 30*time.Second
	defer func() { meshWait, meshDrain = previousWait, previousDrain }()
	server := newTestServer(t, nil)
	meshServer := NewMeshServer(server.service)
	// A hook registered before the one that applies the mesh holds back the change the sync makes
	// (it reports a version of the agent), as a slow hook would, while that sync is being answered.
	versionOf := func(s *State) string {
		if s.Mesh == nil || len(s.Mesh.Nodes) == 0 {
			return ""
		}
		return s.Mesh.Nodes[0].Version
	}
	var armed atomic.Bool
	atGate, openGate := make(chan struct{}), make(chan struct{})
	server.service.OnChange(func(previous, next *State) {
		if armed.Load() && versionOf(next) == "test-2" && versionOf(previous) != "test-2" {
			close(atGate)
			<-openGate
		}
	})
	server.service.OnChange(func(previous, next *State) {
		if err := meshServer.Apply(); err != nil {
			t.Errorf("mesh apply: %v", err)
		}
	})
	t.Cleanup(meshServer.Close)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, nil)
	var created inviteCreated
	server.must(t, http.StatusCreated, "POST", "/mesh/invites", map[string]any{"local": true}, &created)
	inv, _ := meshproto.DecodeInvite(created.Invite)
	agent := newMeshAgent(t, created.Invite)
	agent.join(inv.Token, "web-1")

	armed.Store(true)
	synced := make(chan int, 1)
	go func() {
		data, _ := json.Marshal(meshproto.SyncRequest{Protocol: meshproto.Version, Version: "test-2", Applied: 0, ListenPort: 51820,
			Endpoints: []string{"203.0.113.9:51820"}, Peers: []meshproto.PeerStatus{}})
		request, _ := http.NewRequest(http.MethodPost, agent.url+"/v1/sync", bytes.NewReader(data))
		request.Header.Set("Authorization", "Bearer "+agent.secret)
		response, err := agent.client.Do(request)
		if err != nil {
			t.Errorf("sync: %v", err)
			synced <- 0
			return
		}
		response.Body.Close()
		synced <- response.StatusCode
	}()
	<-atGate // the sync is inside the hooks of its change, with its connection open

	off := make(chan int, 1)
	go func() { off <- server.call(t, "PATCH", "/mesh", map[string]any{"enabled": false}, nil) }()
	time.Sleep(300 * time.Millisecond) // the turning off has closed the port and waits for the sync
	released := time.Now()
	close(openGate)
	select {
	case status := <-off:
		if status != http.StatusOK {
			t.Fatalf("turning the mesh off: status %d", status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("turning the mesh off did not finish")
	}
	if took := time.Since(released); took > 2*time.Second {
		t.Fatalf("turning the mesh off took %v after the sync it waited for could go on: it waited for the server's limit", took)
	}
	if status := <-synced; status != http.StatusOK {
		t.Fatalf("the sync was answered with status %d", status)
	}
}

// A sync whose node is removed while the request is being read leaves no report behind. The report
// is recorded for a node that was removed, after the removal forgot what it knew of it, and nothing
// forgets it again: it stayed in memory for as long as the server ran.
func TestSyncThatCrossesTheRemovalOfItsNodeLeavesNoReport(t *testing.T) {
	server := newTestServer(t, nil)
	meshServer := NewMeshServer(server.service)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, nil)
	var created inviteCreated
	server.must(t, http.StatusCreated, "POST", "/mesh/invites", map[string]any{"local": true}, &created)
	inv, _ := meshproto.DecodeInvite(created.Invite)

	// The agent's requests go straight to the handler: there is no network to wait for.
	serve := func(path string, secret string, body io.Reader) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, body)
		request.RemoteAddr = "192.0.2.10:40000"
		if secret != "" {
			request.Header.Set("Authorization", "Bearer "+secret)
		}
		recorder := httptest.NewRecorder()
		meshServer.ServeHTTP(recorder, request)
		return recorder
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	join, _ := json.Marshal(meshproto.JoinRequest{Protocol: meshproto.Version, Token: inv.Token, Name: "web-1", PublicKey: base64.StdEncoding.EncodeToString(key),
		ListenPort: 51820, Endpoints: []string{"203.0.113.9:51820"}, OS: "linux", Version: "test"})
	var joined meshproto.JoinResponse
	if recorder := serve("/v1/join", "", bytes.NewReader(join)); recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &joined) != nil {
		t.Fatalf("join: %d %s", recorder.Code, recorder.Body.String())
	}

	// The sync is held while its body is read, after the node was authenticated: writing the first byte
	// returns once the handler has read it.
	body, _ := json.Marshal(meshproto.SyncRequest{Protocol: meshproto.Version, Version: "test", ListenPort: 51820,
		Endpoints: []string{"203.0.113.9:51820"}, Peers: []meshproto.PeerStatus{}})
	reader, writer := io.Pipe()
	var answer *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() {
		defer close(done)
		answer = serve("/v1/sync", joined.Secret, reader)
	}()
	if _, err := writer.Write(body[:1]); err != nil {
		t.Fatal(err)
	}
	server.must(t, http.StatusNoContent, "DELETE", "/mesh/nodes/"+joined.NodeID, nil, nil)
	if _, err := writer.Write(body[1:]); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	<-done

	var out meshproto.SyncResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &out); err != nil || answer.Code != http.StatusOK || out.Status != meshproto.StatusRemoved {
		t.Fatalf("the sync of a removed node was answered with %d %s", answer.Code, answer.Body.String())
	}
	if got := server.service.meshLive.Nodes(); got != 0 {
		t.Fatalf("the live view holds a report of %d node(s) after the only node was removed", got)
	}
}

// A mesh port that another program holds is reported, and tried again by the next Apply, which
// every saved change makes: the mesh server comes up once the port is free.
func TestMeshServerTriesAgainWhenItsPortWasBusy(t *testing.T) {
	server := newTestServer(t, nil)
	meshServer := NewMeshServer(server.service)
	t.Cleanup(meshServer.Close)
	var info meshInfo
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, &info)
	busy, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", info.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if err := meshServer.Apply(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("mesh port %d", info.Port)) {
		t.Fatalf("a busy port: %v", err)
	}
	busy.Close()
	if err := meshServer.Apply(); err != nil {
		t.Fatalf("the port is free: %v", err)
	}
	if !listensOn(info.Port) {
		t.Fatal("the mesh server does not listen once its port is free")
	}
	// Turned off, the port closes; turned on again, it opens again.
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": false}, nil)
	if err := meshServer.Apply(); err != nil || listensOn(info.Port) {
		t.Fatalf("after turning the mesh off: %v, listens %v", err, listensOn(info.Port))
	}
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, nil)
	if err := meshServer.Apply(); err != nil || !listensOn(info.Port) {
		t.Fatalf("after turning the mesh on again: %v, listens %v", err, listensOn(info.Port))
	}
}

// A change saved after the mesh server was closed does not open its port again: this is what the
// last accounting pass of a stopping server can do, when a client reaches its data limit.
func TestMeshServerApplyAfterCloseDoesNothing(t *testing.T) {
	server := newTestServer(t, nil)
	meshServer := NewMeshServer(server.service)
	server.service.OnChange(func(previous, next *State) {
		if err := meshServer.Apply(); err != nil {
			t.Errorf("mesh apply: %v", err)
		}
	})
	t.Cleanup(meshServer.Close)
	var info meshInfo
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, &info)
	if !listensOn(info.Port) {
		t.Fatal("the mesh server does not listen while the mesh is on")
	}
	meshServer.Close()
	if listensOn(info.Port) {
		t.Fatal("the mesh server still listens after Close")
	}
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"ping": false}, nil)
	if err := meshServer.Apply(); err != nil {
		t.Fatal(err)
	}
	if listensOn(info.Port) {
		t.Fatal("a change saved after Close opened the mesh port again")
	}
}

// Apply calls made at once act on the state in the order they get their turn, so the last one to
// act has read the state last. The first call here has read the mesh port that was saved first and is
// held; a second saves a newer one and applies it; the first then goes on. Before, the first one
// went on with what it had read and put the older port in place of the newer.
func TestMeshApplyActsOnTheStateAsItIsWhenItsTurnComes(t *testing.T) {
	server := newTestServer(t, nil)
	meshServer := NewMeshServer(server.service)
	t.Cleanup(meshServer.Close)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, nil)
	if err := meshServer.Apply(); err != nil {
		t.Fatal(err)
	}
	setPort := func(port uint16) {
		if err := server.service.Change(func(s *State) error { s.Mesh.Port = port; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	older := server.unreservedPort(t)
	setPort(older)
	var held atomic.Bool
	atHook, proceed := make(chan struct{}), make(chan struct{})
	meshServer.afterRead = func() {
		if held.CompareAndSwap(false, true) {
			close(atHook)
			<-proceed
		}
	}
	done := make(chan error, 2)
	go func() { done <- meshServer.Apply() }()
	<-atHook // the first Apply has read the older port
	newer := server.unreservedPort(t)
	setPort(newer)
	go func() { done <- meshServer.Apply() }()
	time.Sleep(300 * time.Millisecond) // the second Apply is under way; the first one is held
	close(proceed)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if !listensOn(newer) || listensOn(older) {
		t.Fatalf("the newer port %d listens: %v; the older port %d listens: %v", newer, listensOn(newer), older, listensOn(older))
	}
}

// Choosing the network of a mesh that was never turned on does not turn it on either: only
// "enabled" does. The network chosen is the one the mesh gets when it is turned on.
func TestMeshChoosingANetworkFirstDoesNotTurnItOn(t *testing.T) {
	server := newTestServer(t, nil)
	var info meshInfo
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"cidr": "10.77.0.0/16"}, &info)
	if info.Enabled || info.CIDR != "10.77.0.0/16" {
		t.Fatalf("a network chosen before the mesh exists: %+v", info)
	}
	server.service.Read(func(s *State) {
		if ports := s.Listening(); ports["mesh"] != "" {
			t.Errorf("the mesh listens although it is off: %v", ports)
		}
	})
	if status := server.call(t, "POST", "/mesh/invites", map[string]any{}, nil); status != http.StatusBadRequest {
		t.Fatalf("an invite for a mesh that is off: status %d", status)
	}
	// Turned on, it has the network that was chosen, and keeps the port it was given.
	var on meshInfo
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, &on)
	if !on.Enabled || on.CIDR != "10.77.0.0/16" || on.ID != info.ID || on.Port != info.Port {
		t.Fatalf("turned on after choosing a network: %+v, before: %+v", on, info)
	}
	// With "enabled" in the same request, it is on at once.
	other := newTestServer(t, nil)
	other.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true, "cidr": "10.78.0.0/16"}, &on)
	if !on.Enabled || on.CIDR != "10.78.0.0/16" {
		t.Fatalf("enabled with a network: %+v", on)
	}
}

// Choosing another network does not turn a mesh that is off back on.
func TestMeshNetworkChangeKeepsItOff(t *testing.T) {
	server := newTestServer(t, nil)
	var info meshInfo
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, nil)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": false}, nil)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"cidr": "10.77.0.0/16"}, &info)
	if info.Enabled || info.CIDR != "10.77.0.0/16" {
		t.Fatalf("new network on a mesh that is off: %+v", info)
	}
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": true}, nil)
	server.must(t, http.StatusOK, "PATCH", "/mesh", map[string]any{"enabled": false, "cidr": "10.78.0.0/16"}, &info)
	if info.Enabled || info.CIDR != "10.78.0.0/16" {
		t.Fatalf("turned off with a new network: %+v", info)
	}
}
