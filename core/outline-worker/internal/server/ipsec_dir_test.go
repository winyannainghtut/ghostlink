// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/ipsecproto"
)

// The files shared with the IPsec agent are in <state>/ipsec unless the server is told another
// directory (--ipsec-dir). On a Linux server the host script tells it one outside the state, made
// and owned by root, so that the server account cannot replace the directory that Docker mounts
// into the IPsec component. These tests keep both: the default is what every other test and a
// development run use, and the named directory gets both files, and nothing is left in the default.

func ikev2On(t *testing.T, server *testServer) {
	t.Helper()
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"protocols": map[string]any{"ikev2": map[string]bool{"enabled": true}}}, nil)
}

func reportIn(t *testing.T, dir string, running bool) {
	t.Helper()
	data, _ := json.Marshal(ipsecproto.Status{Version: 1, UpdatedUnix: time.Now().Unix(), Running: running, SAs: []ipsecproto.SA{}, Plugins: []string{"vici"}})
	if err := os.WriteFile(filepath.Join(dir, ipsecproto.StatusFile), data, 0o640); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestIPsecDirIsInTheStateByDefault(t *testing.T) {
	server := newTestServer(t, nil)
	fakeAgent(t, server, nil) // turning a protocol on needs a running component
	ikev2On(t, server)
	state := server.store.Dir()
	if !exists(filepath.Join(state, IPsecDir, ipsecproto.DesiredFile)) {
		t.Fatalf("the settings are not in %s", filepath.Join(state, IPsecDir))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(state, IPsecDir))
		if err != nil || info.Mode().Perm() != 0o770 {
			t.Fatalf("the default directory is %v (%v), want 0770", info, err)
		}
	}
	if dir, inState := server.service.ipsecPath(); !inState || dir != filepath.Join(state, IPsecDir) {
		t.Fatalf("the default path is %q (in the state: %v)", dir, inState)
	}
	// SetIPsecDir("") is the default as well.
	server.service.SetIPsecDir(filepath.Join(t.TempDir(), "elsewhere"))
	server.service.SetIPsecDir("")
	if dir, inState := server.service.ipsecPath(); !inState || dir != filepath.Join(state, IPsecDir) {
		t.Fatalf("after SetIPsecDir(\"\") the path is %q (in the state: %v)", dir, inState)
	}
}

func TestIPsecDirNamedByTheHostGetsTheSettingsAndTheReport(t *testing.T) {
	server := newTestServer(t, nil)
	exchange := t.TempDir()
	server.service.SetIPsecDir(exchange)
	state := server.store.Dir()

	// Turning a protocol on needs a running component: the server asks the given directory for it.
	reportIn(t, exchange, true)
	// The settings are written into the named directory, and nothing is made in the state.
	ikev2On(t, server)
	data, err := os.ReadFile(filepath.Join(exchange, ipsecproto.DesiredFile))
	if err != nil {
		t.Fatalf("the settings are not in the directory the server was given: %v", err)
	}
	var desired ipsecproto.Desired
	if err := json.Unmarshal(data, &desired); err != nil || desired.Validate() != nil || desired.IKEv2 == nil {
		t.Fatalf("settings in the given directory: %v %v %s", err, desired.Validate(), data)
	}
	if exists(filepath.Join(state, IPsecDir)) {
		t.Fatalf("%s was made although the server was given another directory", filepath.Join(state, IPsecDir))
	}
	// A change goes there too.
	user := server.user(t, map[string]any{"name": "Phone"})
	key := server.appleKey(t, user, map[string]string{"protocol": "ikev2"})
	data, _ = os.ReadFile(filepath.Join(exchange, ipsecproto.DesiredFile))
	desired = ipsecproto.Desired{}
	if err := json.Unmarshal(data, &desired); err != nil || len(desired.Allowed) != 1 || desired.Allowed[0] != key.Identity {
		t.Fatalf("a new key did not reach the given directory: %v %v", err, desired.Allowed)
	}

	// The report is read from the named directory only: one in the state's own ipsec directory
	// (where a server of 0.9.1 and earlier, or an account that wants to mislead it, would put one)
	// does not count.
	reportIn(t, exchange, false) // the component in the given directory has stopped
	if err := os.MkdirAll(filepath.Join(state, IPsecDir), 0o770); err != nil {
		t.Fatal(err)
	}
	reportIn(t, filepath.Join(state, IPsecDir), true)
	if _, running := server.service.IPsecStatus(time.Now()); running {
		t.Fatal("a report in the state's ipsec directory counts although the server was given another directory")
	}
	var info struct {
		Component struct {
			Running bool `json:"running"`
		} `json:"ipsec_component"`
	}
	server.must(t, http.StatusOK, "GET", "/server", nil, &info)
	if info.Component.Running {
		t.Fatal("the API shows the component running on a report in the wrong directory")
	}
	reportIn(t, exchange, true)
	if _, running := server.service.IPsecStatus(time.Now()); !running {
		t.Fatal("a fresh report in the given directory does not count")
	}
	server.must(t, http.StatusOK, "GET", "/server", nil, &info)
	if !info.Component.Running {
		t.Fatal("the API (and with it every admin command) does not show the component running on the report in the given directory")
	}
	reportIn(t, exchange, false)
	if _, running := server.service.IPsecStatus(time.Now()); running {
		t.Fatal("a report that says it is not running counts")
	}
}

func TestIPsecDirNamedByTheHostIsNeitherMadeNorChanged(t *testing.T) {
	server := newTestServer(t, nil)

	// A directory that is missing is an error that is reported, and it is not made: the host
	// script owns it, and a directory this account made would be the account's to replace.
	missing := filepath.Join(t.TempDir(), "ghostlink-ipsec")
	server.service.SetIPsecDir(missing)
	if err := server.service.WriteIPsecDesired(time.Now()); err == nil {
		t.Fatal("the settings were written although the directory is missing")
	}
	if exists(missing) {
		t.Fatalf("the server made %s", missing)
	}

	// A file in its place is refused.
	if err := os.WriteFile(missing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.service.WriteIPsecDesired(time.Now()); err == nil {
		t.Fatal("the settings were written although a file is in the place of the directory")
	}

	// A directory that is there keeps its owner and its mode (setgid, group of the account): the
	// server does not chmod it, which would fail for a directory it does not own and would drop
	// the bit that keeps the agent's files in the account's group.
	exchange := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(exchange, 0o750|os.ModeSetgid); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(exchange)
	if err != nil {
		t.Fatal(err)
	}
	server.service.SetIPsecDir(exchange)
	if err := server.service.WriteIPsecDesired(time.Now()); err != nil {
		t.Fatalf("a directory that is there was refused: %v", err)
	}
	after, err := os.Stat(exchange)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() {
		t.Fatalf("the mode of the directory changed from %v to %v", before.Mode(), after.Mode())
	}
	if runtime.GOOS != "windows" && after.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("the setgid bit of the directory is gone: %v", after.Mode())
	}
	if !exists(filepath.Join(exchange, ipsecproto.DesiredFile)) {
		t.Fatal("no settings in the directory")
	}
}
