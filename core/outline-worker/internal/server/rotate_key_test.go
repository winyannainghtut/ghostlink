// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rotated is the answer of POST /server/rotate-key.
type rotated struct {
	Key ManagementKey `json:"management_key"`
}

func secretOf(key ManagementKey) string {
	return key.APIURL[strings.LastIndex(key.APIURL, "/")+1:]
}

// The old key stops working at once, the new one works at once, the same certificate is kept, the
// new key is saved (state and key file, so it survives a restart) and the VPN engine is not
// restarted, so connected clients stay connected.
func TestRotateKeyReplacesTheManagementKey(t *testing.T) {
	server := newTestServer(t, nil)
	server.user(t, map[string]any{"name": "Alice"})
	oldSecret := server.store.State.API.Secret
	reloads := server.service.Health().EngineReloads
	fingerprint, err := server.store.State.CertSHA256()
	if err != nil {
		t.Fatal(err)
	}

	var answer rotated
	server.must(t, http.StatusOK, "POST", "/server/rotate-key", nil, &answer)
	newSecret := secretOf(answer.Key)
	if newSecret == "" || newSecret == oldSecret || len(newSecret) != len(oldSecret) {
		t.Fatalf("secret %q after %q", newSecret, oldSecret)
	}
	if !strings.HasSuffix(answer.Key.APIURL, "/"+newSecret) || !strings.HasPrefix(answer.Key.APIURL, "https://127.0.0.1:") {
		t.Fatalf("API address %q", answer.Key.APIURL)
	}
	if answer.Key.CertSHA256 != strings.ToUpper(fingerprint) {
		t.Fatalf("the certificate changed: %q, was %q", answer.Key.CertSHA256, fingerprint)
	}

	// The key that was used for the request is now refused; the new one works.
	if status := server.call(t, "GET", "/server", nil, nil); status != http.StatusNotFound {
		t.Fatalf("the old key still works: status %d", status)
	}
	server.base = server.api.URL + "/" + newSecret
	var info serverInfo
	server.must(t, http.StatusOK, "GET", "/server", nil, &info)
	if info.Users != 1 {
		t.Fatalf("users after the rotation: %d", info.Users)
	}

	// Saved: in the state on disk and in the key file `show-key` prints.
	loaded, err := Load(server.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State.API.Secret != newSecret {
		t.Fatalf("saved secret %q, want %q", loaded.State.API.Secret, newSecret)
	}
	file, err := os.ReadFile(filepath.Join(server.store.Dir(), ManagementKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	var written ManagementKey
	if err := json.Unmarshal(file, &written); err != nil || written != answer.Key {
		t.Fatalf("key file %s (%v), want %+v", file, err, answer.Key)
	}

	// Nothing the clients depend on moved.
	if got := server.service.Health().EngineReloads; got != reloads {
		t.Fatalf("the engine restarted %d time(s) for a new management key", got-reloads)
	}

	// And it can be done again, from the new key.
	var again rotated
	server.must(t, http.StatusOK, "POST", "/server/rotate-key", nil, &again)
	if secretOf(again.Key) == newSecret {
		t.Fatal("a second rotation gave the same secret")
	}
	if status := server.call(t, "GET", "/server", nil, nil); status != http.StatusNotFound {
		t.Fatalf("the key before the second rotation still works: status %d", status)
	}
}

// Requests that arrive while the key is rotated are answered with either key's rules, never with
// a crash or a race: run with -race.
func TestRotateKeyWhileRequestsArrive(t *testing.T) {
	server := newTestServer(t, nil)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				response, err := http.Get(server.base + "/server")
				if err == nil {
					response.Body.Close()
				}
			}
		}
	}()
	for i := 0; i < 5; i++ {
		var answer rotated
		server.must(t, http.StatusOK, "POST", "/server/rotate-key", nil, &answer)
		server.base = server.api.URL + "/" + secretOf(answer.Key)
	}
	close(stop)
	<-done
}

// rotateNow runs one rotation of api, as its handler does, and returns the key it answers with.
func rotateNow(t *testing.T, api *API) ManagementKey {
	t.Helper()
	status, body, err := api.rotateKey(nil)
	if err != nil || status != http.StatusOK {
		t.Errorf("rotation: %d %v", status, err)
		return ManagementKey{}
	}
	return body.(map[string]any)["management_key"].(ManagementKey)
}

// keyInUse is the secret the API accepts, the one in the saved state and the one in the key file.
func keyInUse(t *testing.T, server *testServer, api *API) (inUse, saved, filed string) {
	t.Helper()
	loaded, err := Load(server.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(filepath.Join(server.store.Dir(), ManagementKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return api.secretNow(), loaded.State.API.Secret, secretOf(parseKey(t, file))
}

// Two rotations that overlap end with one key, the later one: the key that works is the one in the
// saved state and in the key file, and it is the key the later caller was given. The first rotation
// is held back after it saved its secret; before, the second one then ran to its end, and the first
// put its older secret in use afterwards: the management key that worked was not the one anyone had
// been given last, and not the one that survives a restart.
func TestOverlappingRotationsEndWithOneKey(t *testing.T) {
	server := newTestServer(t, nil)
	api := NewAPI(server.service, "test")
	var held atomic.Bool
	saved, release := make(chan struct{}), make(chan struct{})
	api.afterRotationSave = func() {
		if held.CompareAndSwap(false, true) {
			close(saved)
			<-release
		}
	}
	var first, second ManagementKey
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); first = rotateNow(t, api) }()
	<-saved
	wg.Add(1)
	go func() { defer wg.Done(); second = rotateNow(t, api) }()
	// The second rotation is started while the first is between saving and using its secret. It
	// finishes first if it can; give it the time.
	time.Sleep(300 * time.Millisecond)
	close(release)
	wg.Wait()

	inUse, savedSecret, filed := keyInUse(t, server, api)
	if inUse != savedSecret || savedSecret != filed {
		t.Fatalf("the key in use is %q, the saved state has %q and the key file %q", inUse, savedSecret, filed)
	}
	if secretOf(first) == secretOf(second) || inUse != secretOf(second) {
		t.Fatalf("the key in use is %q; the first rotation answered %q and the second, the later one, %q", inUse, secretOf(first), secretOf(second))
	}
}

// Many rotations at once leave the key in use, the saved state and the key file the same.
func TestManyRotationsAtOnceAgree(t *testing.T) {
	server := newTestServer(t, nil)
	api := NewAPI(server.service, "test")
	var wg sync.WaitGroup
	for caller := 0; caller < 6; caller++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				rotateNow(t, api)
			}
		}()
	}
	wg.Wait()
	inUse, saved, filed := keyInUse(t, server, api)
	if inUse != saved || saved != filed {
		t.Fatalf("the key in use is %q, the saved state has %q and the key file %q", inUse, saved, filed)
	}
}

// `ghostlink-server rotate-key`, on the server: it prints the new key and the old one in the key
// file is replaced; it takes no arguments, and --json prints the key for scripts.
func TestAdminRotateKey(t *testing.T) {
	server := newTestServer(t, func(s *State) { s.API.Port = freeTCPPort(t) })
	run := adminCommands(t, server)
	before, err := os.ReadFile(filepath.Join(server.store.Dir(), ManagementKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run("rotate-key", "now"); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("an argument was accepted: %v", err)
	}
	out, err := run("rotate-key")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(server.store.Dir(), ManagementKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) {
		t.Fatal("the key file was not replaced")
	}
	if !strings.Contains(out, strings.TrimSpace(string(after))) || !strings.Contains(out, "no longer works") {
		t.Fatalf("output lacks the new key:\n%s", out)
	}
	if strings.Contains(out, secretOf(parseKey(t, before))) {
		t.Fatalf("output still shows the old secret:\n%s", out)
	}
	// The next command uses the new key from the file, as a new invocation does.
	if out, err := run("--json", "rotate-key"); err != nil || !strings.Contains(out, `"management_key"`) {
		t.Fatalf("--json: %v\n%s", err, out)
	}
	if _, err := run("users"); err != nil {
		t.Fatalf("a command after two rotations: %v", err)
	}
}

func parseKey(t *testing.T, data []byte) ManagementKey {
	t.Helper()
	var key ManagementKey
	if err := json.Unmarshal(data, &key); err != nil {
		t.Fatal(err)
	}
	return key
}
