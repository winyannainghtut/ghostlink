// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ghostlink.local/outline-worker/internal/asn"
)

// The ranges and AS numbers are the ones set aside for documentation (RFC 5737, RFC 3849 and
// RFC 5398): no real network is named in these tests.
const testASNList = "192.0.2.0\t192.0.2.255\t64496\tZZ\tEXAMPLE-ONE - Example Telecom One, Inc.\n" +
	"198.51.100.0\t198.51.100.255\t64498\tZZ\tEXAMPLE-THREE - Example Three\n" +
	"203.0.113.0\t203.0.113.255\t0\tNone\tNot routed\n" +
	"2001:db8::\t2001:db8::ffff\t64497\tZZ\tEXAMPLE-TWO - Example Mobile Two\n"

func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// asnSource is a stand-in for iptoasn.com on the local machine: what it answers can be changed,
// and an answer can be held back until the test lets it go.
type asnSource struct {
	mu     sync.Mutex
	status int
	body   []byte
	hold   chan struct{}
	hits   atomic.Int32
	// abandoned counts the held requests the downloader gave up (it went away before the answer).
	abandoned atomic.Int32
}

func serveASN(t *testing.T, body []byte) *asnSource {
	t.Helper()
	source := &asnSource{status: http.StatusOK, body: body}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source.hits.Add(1)
		source.mu.Lock()
		status, body, hold := source.status, source.body, source.hold
		source.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				source.abandoned.Add(1)
				return
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	oldURL, oldHTTP, oldMin, oldPoll := asnSourceURL, asnAllowHTTP, asnMinRanges, asnPoll
	asnSourceURL, asnAllowHTTP, asnMinRanges, asnPoll = httpServer.URL+"/ip2asn-combined.tsv.gz", true, 3, 10*time.Millisecond
	t.Cleanup(func() {
		httpServer.Close()
		asnSourceURL, asnAllowHTTP, asnMinRanges, asnPoll = oldURL, oldHTTP, oldMin, oldPoll
	})
	return source
}

func (s *asnSource) answer(status int, body []byte) {
	s.mu.Lock()
	s.status, s.body = status, body
	s.mu.Unlock()
}

// holdAnswers keeps every request waiting until the returned function is called (a request whose
// sender goes away stops waiting).
func (s *asnSource) holdAnswers() (release func()) {
	hold := make(chan struct{})
	s.mu.Lock()
	s.hold = hold
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(hold) }) }
}

func mustTable(t *testing.T) *asn.Table {
	t.Helper()
	table, err := asn.Parse(strings.NewReader(testASNList))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func (s *testServer) asnStatus(t *testing.T) asnStatus {
	t.Helper()
	var info serverInfo
	s.must(t, http.StatusOK, "GET", "/server", nil, &info)
	return info.ASN
}

// seenFrom makes the monitor record that user (an ID) connected from address.
func seenFrom(t *testing.T, server *testServer, user, address string) {
	t.Helper()
	server.service.monitor.AddExternal("u"+user, IKEv2, 10, 10, address, time.Now())
	server.service.Record(time.Now())
}

func (s *testServer) clients(t *testing.T) (map[string]clientView, asnStatus) {
	t.Helper()
	var out struct {
		Clients []clientView `json:"clients"`
		ASN     asnStatus    `json:"asn"`
	}
	s.must(t, http.StatusOK, "GET", "/clients", nil, &out)
	byIP := map[string]clientView{}
	for _, c := range out.Clients {
		byIP[c.IP] = c
	}
	return byIP, out.ASN
}

// Turned on, the lookup shows who runs each address; until then it shows nothing, and asks nobody.
func TestASNLookupIsOffUntilTurnedOn(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	alice := server.user(t, map[string]any{"name": "Alice"})
	seenFrom(t, server, alice.ID, "192.0.2.10")

	byIP, status := server.clients(t)
	if status.Enabled || status.Ready || byIP["192.0.2.10"].ASN != 0 || byIP["192.0.2.10"].Operator != "" {
		t.Fatalf("a provider while the lookup is off: %+v %+v", byIP["192.0.2.10"], status)
	}
	if server.asnStatus(t).Enabled {
		t.Fatal("the lookup is on in a new server")
	}
	time.Sleep(100 * time.Millisecond)
	if source.hits.Load() != 0 {
		t.Fatalf("the list was downloaded %d time(s) while the lookup is off", source.hits.Load())
	}
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile); err == nil {
		t.Fatal("a list is saved while the lookup is off")
	}
}

func TestASNLookupShowsTheProviderOfAClient(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	alice := server.user(t, map[string]any{"name": "Alice"})
	for _, address := range []string{"192.0.2.10", "2001:db8::5", "10.1.2.3", "203.0.113.9", "198.51.100.7"} {
		seenFrom(t, server, alice.ID, address)
	}

	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the list", func() bool { return server.asnStatus(t).Ready })
	status := server.asnStatus(t)
	if !status.Enabled || status.Updating || status.Error != "" || status.Ranges != 3 || status.Source != ASNSource || status.UpdatedUnix == 0 {
		t.Fatalf("status after the download: %+v", status)
	}
	if source.hits.Load() != 1 {
		t.Fatalf("downloaded %d times", source.hits.Load())
	}

	byIP, listed := server.clients(t)
	if !listed.Ready {
		t.Fatalf("/clients does not say the lookup is ready: %+v", listed)
	}
	for address, want := range map[string]struct {
		number   uint32
		operator string
	}{
		"192.0.2.10":   {64496, "Example Telecom One, Inc."},
		"2001:db8::5":  {64497, "Example Mobile Two"},
		"198.51.100.7": {64498, "Example Three"},
		"10.1.2.3":     {}, // a private address is nobody's
		"203.0.113.9":  {}, // not routed
	} {
		got, ok := byIP[address]
		if !ok {
			t.Fatalf("%s is not listed", address)
		}
		if got.ASN != want.number || got.Operator != want.operator {
			t.Errorf("%s: AS%d %q, want AS%d %q", address, got.ASN, got.Operator, want.number, want.operator)
		}
	}
	// Saved for the next start, and counted among the server's data.
	if info, err := os.Stat(server.store.Dir() + "/" + ASNFile); err != nil || info.Size() == 0 {
		t.Fatalf("the list was not saved: %v", err)
	}
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile + ".pending"); err == nil {
		t.Fatal("the download left its temporary file")
	}
}

// What a server that is not iptoasn.com, or iptoasn.com on a bad day, may send: none of it is
// used, the error is said, and nothing is left on disk.
func TestASNBadListsAreRefused(t *testing.T) {
	good := gzipped(t, testASNList)
	for name, c := range map[string]struct {
		status int
		body   []byte
		want   string
	}{
		"rate limited":       {http.StatusTooManyRequests, []byte("slow down"), "429"},
		"server error":       {http.StatusInternalServerError, []byte("oops"), "500"},
		"html in place":      {http.StatusOK, gzipped(t, "<html>Please log in</html>\n"), "ip2asn format"},
		"not gzip at all":    {http.StatusOK, []byte("192.0.2.0\t192.0.2.255\t64496\tZZ\tX - Y\n"), "gzip"},
		"cut off":            {http.StatusOK, good[:len(good)/2], "refused"},
		"too few ranges":     {http.StatusOK, gzipped(t, "192.0.2.0\t192.0.2.255\t64496\tZZ\tX - Y\n"), "only 1 ranges"},
		"empty":              {http.StatusOK, nil, "refused"},
		"expands enormously": {http.StatusOK, gzipped(t, strings.Repeat("192.0.2.0\t192.0.2.255\t64496\tZZ\tX - Y\n", 50_000)), "expands to more than"},
	} {
		t.Run(name, func(t *testing.T) {
			source := serveASN(t, good)
			source.answer(c.status, c.body)
			if name == "expands enormously" {
				old := asnMaxExpanded
				asnMaxExpanded = 1 << 20
				t.Cleanup(func() { asnMaxExpanded = old })
			}
			server := newTestServer(t, nil)
			server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
			waitFor(t, "the try to end", func() bool {
				s := server.asnStatus(t)
				return !s.Updating && s.Error != ""
			})
			status := server.asnStatus(t)
			if status.Ready || !strings.Contains(status.Error, c.want) {
				t.Fatalf("status %+v, want an error with %q and no list", status, c.want)
			}
			for _, file := range []string{ASNFile, ASNFile + ".pending"} {
				if _, err := os.Stat(server.store.Dir() + "/" + file); err == nil {
					t.Fatalf("%s was left on disk", file)
				}
			}
		})
	}
}

// A download that fails later does not take away the list that works.
func TestASNFailedUpdateKeepsTheListInUse(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	alice := server.user(t, map[string]any{"name": "Alice"})
	seenFrom(t, server, alice.ID, "192.0.2.10")
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the list", func() bool { return server.asnStatus(t).Ready })

	source.answer(http.StatusBadGateway, []byte("bad gateway"))
	var accepted asnStatus
	server.must(t, http.StatusAccepted, "POST", "/asn/update", nil, &accepted)
	waitFor(t, "the failed update", func() bool {
		s := server.asnStatus(t)
		return !s.Updating && s.Error != ""
	})
	status := server.asnStatus(t)
	if !status.Ready || !strings.Contains(status.Error, "502") {
		t.Fatalf("status after a failed update: %+v", status)
	}
	if byIP, _ := server.clients(t); byIP["192.0.2.10"].ASN != 64496 {
		t.Fatalf("the list in use was lost: %+v", byIP["192.0.2.10"])
	}

	// And the next good answer clears the error.
	source.answer(http.StatusOK, gzipped(t, strings.Replace(testASNList, "Example Telecom One, Inc.", "Renamed Telecom", 1)))
	server.must(t, http.StatusAccepted, "POST", "/asn/update", nil, nil)
	waitFor(t, "the new list", func() bool {
		s := server.asnStatus(t)
		return !s.Updating && s.Error == ""
	})
	if byIP, _ := server.clients(t); byIP["192.0.2.10"].Operator != "Renamed Telecom" {
		t.Fatalf("the new list is not in use: %+v", byIP["192.0.2.10"])
	}
}

func TestASNUpdateNeedsTheLookupOn(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	server.must(t, http.StatusBadRequest, "POST", "/asn/update", nil, nil)
	if !strings.Contains(server.last, "turn it on first") {
		t.Fatalf("answer: %s", server.last)
	}
	if source.hits.Load() != 0 {
		t.Fatal("a download started while the lookup is off")
	}
}

// Turning it off deletes the list, and a download that was still running does not bring it back.
func TestASNTurnedOffDeletesTheListAndStopsADownloadInFlight(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	alice := server.user(t, map[string]any{"name": "Alice"})
	seenFrom(t, server, alice.ID, "192.0.2.10")
	path := server.store.Dir() + "/" + ASNFile

	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the list", func() bool { return server.asnStatus(t).Ready })
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": false}, nil)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the list was kept after the lookup was turned off")
	}
	if byIP, status := server.clients(t); status.Enabled || byIP["192.0.2.10"].ASN != 0 {
		t.Fatalf("a provider after turning it off: %+v %+v", byIP["192.0.2.10"], status)
	}

	// Turned on, and off again while the answer is on its way.
	source.mu.Lock()
	source.hold = make(chan struct{})
	hold := source.hold
	source.mu.Unlock()
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the download to begin", func() bool { return source.hits.Load() == 2 })
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": false}, nil)
	close(hold)
	waitFor(t, "the download to end", func() bool {
		server.service.asn.mu.Lock()
		defer server.service.asn.mu.Unlock()
		return !server.service.asn.busy
	})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a download that began before the lookup was turned off put its list back")
	}
	if _, found := server.service.asn.Lookup(netip.MustParseAddr("192.0.2.10")); found {
		t.Fatal("the list of a download that began before the lookup was turned off is in use")
	}
	if status := server.asnStatus(t); status.Enabled || status.Error != "" {
		t.Fatalf("status while off: %+v", status)
	}
}

// Turning it off ends a download that is running at once (its connection is dropped), instead of
// leaving it to run on, up to the five minutes its timeout allows, only to throw the list away.
func TestASNTurnedOffEndsTheDownloadAtOnce(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	release := source.holdAnswers()
	defer release()
	server := newTestServer(t, nil)

	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the download to begin", func() bool { return source.hits.Load() == 1 })
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": false}, nil)

	waitFor(t, "the download to be given up", func() bool { return source.abandoned.Load() == 1 })
	waitFor(t, "the download to end", func() bool {
		server.service.asn.mu.Lock()
		defer server.service.asn.mu.Unlock()
		return !server.service.asn.busy
	})
	if status := server.asnStatus(t); status.Enabled || status.Error != "" || status.Updating {
		t.Fatalf("status after turning it off: %+v", status)
	}
	// The next time it is turned on is a clean start: a download, not a refusal.
	release()
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the list", func() bool { return server.asnStatus(t).Ready })
	if status := server.asnStatus(t); status.Error != "" || status.Updating || source.hits.Load() != 2 {
		t.Fatalf("status after turning it on again: %+v, %d downloads", status, source.hits.Load())
	}
}

// Turned off and on again while the first download is still running, the lookup gets its list: the
// download of the first time is ended, and one is started for the second. Before, the second was
// refused because the first still ran, which then failed with "turned off while the list was
// downloading": the lookup stayed on with no list, and that error, until the next hourly check.
func TestASNTurnedOffAndOnAgainWhileDownloadingGetsTheList(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	release := source.holdAnswers()
	defer release()
	server := newTestServer(t, nil)
	alice := server.user(t, map[string]any{"name": "Alice"})
	seenFrom(t, server, alice.ID, "192.0.2.10")

	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the first download to begin", func() bool { return source.hits.Load() == 1 })
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": false}, nil)
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	release()

	waitFor(t, "the list", func() bool { return server.asnStatus(t).Ready })
	waitFor(t, "the downloads to end", func() bool { return !server.asnStatus(t).Updating })
	status := server.asnStatus(t)
	if !status.Enabled || status.Error != "" || status.Ranges != 3 {
		t.Fatalf("status: %+v", status)
	}
	if byIP, _ := server.clients(t); byIP["192.0.2.10"].ASN != 64496 {
		t.Fatalf("the list is not in use: %+v", byIP["192.0.2.10"])
	}
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile); err != nil {
		t.Fatalf("the list was not saved: %v", err)
	}
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile + ".pending"); err == nil {
		t.Fatal("a download left its temporary file")
	}
}

// Closing the service ends a download in flight instead of waiting for it, and none begins after
// that.
func TestASNClosingTheServiceEndsTheDownloadAndStartsNoOther(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	source.holdAnswers() // never released
	server := newTestServer(t, nil)
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the download to begin", func() bool { return source.hits.Load() == 1 })
	closed := make(chan struct{})
	go func() {
		_ = server.service.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("closing the service waited for the download")
	}
	// The source sees the request go away a moment after Close has ended it (the connection closes
	// asynchronously), so it is waited for: checked at once, it failed once in CI (0 given up). A
	// download that Close did not end is held for ever, and the wait times out.
	waitFor(t, "the held request to be given up", func() bool { return source.abandoned.Load() == 1 })
	if !server.service.RefreshASN(time.Now()) {
		t.Fatal("the setting is on, so a refresh is accepted")
	}
	server.service.refreshASNIfDue(time.Now())
	time.Sleep(200 * time.Millisecond)
	if source.hits.Load() != 1 {
		t.Fatalf("a download began after the service was closed (%d requests)", source.hits.Load())
	}
}

// A download that was ended because the lookup was turned off leaves nothing behind for the next
// one: no error, and no wait before a new download is due. Before, it recorded its own failure when
// it ended, which the lookup turned on again then showed, and the retry window it left made the
// next try wait.
func TestASNEndedDownloadLeavesNoTraceForTheNextOne(t *testing.T) {
	manager := newASNManager(t.TempDir())
	now := time.Now()
	ctx, generation, ok := manager.begin(t.Context(), now)
	if !ok {
		t.Fatal("begin")
	}
	manager.Drop() // the lookup is turned off while the download runs
	if ctx.Err() == nil {
		t.Fatal("turning the lookup off did not end the download")
	}
	// It ends with the error of having been cut off, when the lookup is on again.
	if counts := manager.finish(generation, ctx.Err()); counts {
		t.Fatal("a download ended by turning the lookup off counts as the current one")
	}
	if status := manager.Status(true); status.Error != "" || status.Updating || status.Ready {
		t.Fatalf("status after it ended: %+v", status)
	}
	if !manager.Due(now.Add(time.Second)) {
		t.Fatal("a new download is not due, although the one that was ended never counted")
	}
	// A download begun after that is the current one, and its failure counts as one.
	_, next, ok := manager.begin(t.Context(), now.Add(time.Second))
	if !ok || next == generation {
		t.Fatalf("the next download: generation %d after %d, begun %v", next, generation, ok)
	}
	if !manager.finish(next, os.ErrDeadlineExceeded) || manager.Status(true).Error == "" {
		t.Fatal("the failure of the current download was not recorded")
	}
}

// A download is begun only while the lookup is on, and that is checked where it begins: a refresh
// that finds the lookup off does nothing, and says so. This is what the hooks of changes that
// finished in another order than they ran in, and an update asked for as the lookup is turned
// off, depend on.
func TestASNRefreshDoesNothingWhileTheLookupIsOff(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	if server.service.RefreshASN(time.Now()) {
		t.Fatal("a refresh was accepted with the lookup off")
	}
	server.service.refreshASNIfDue(time.Now())
	// A hook that runs after the setting went off again must not start what the hook of the change
	// before it asked for.
	server.service.syncASN(time.Now())
	time.Sleep(200 * time.Millisecond)
	if source.hits.Load() != 0 || server.asnStatus(t).Updating {
		t.Fatalf("a download began with the lookup off: %d request(s)", source.hits.Load())
	}
	server.must(t, http.StatusBadRequest, "POST", "/asn/update", nil, nil)
	if source.hits.Load() != 0 {
		t.Fatal("an update asked for while the lookup is off began a download")
	}

	// The same through the setting: on, and the hook of "on" runs while the setting is already off
	// again (done here by hand, as two requests at once can finish in that order).
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the list", func() bool { return server.asnStatus(t).Ready })
	server.store.Lock()
	server.store.State.Monitor.ASNLookup = false
	server.store.Unlock()
	server.service.syncASN(time.Now()) // the hook of the change that turned it on, running late
	if _, found := server.service.asn.Lookup(netip.MustParseAddr("192.0.2.10")); found {
		t.Fatal("the list is in use after the setting went off")
	}
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile); err == nil {
		t.Fatal("the list is on disk after the setting went off")
	}
}

// Many changes of the setting at once, each running its hook in any order, leave the lookup as the
// setting last was: no list, no download and no file when it ended off; the list, and no error, when
// it ended on. Each caller ends with the same setting, so whichever finishes last, that is the last
// setting; the hooks of the changes before it run in any order, also after the last one.
func TestASNManyTogglesAtOnceEndWhereTheSettingEnds(t *testing.T) {
	for _, ends := range []bool{false, true} {
		name := "ends off"
		if ends {
			name = "ends on"
		}
		t.Run(name, func(t *testing.T) { toggleAtOnce(t, ends) })
	}
}

// The hook of a change that turned the lookup on runs after the change that turned it off again
// (changes made at once finish in any order): it follows the setting as it is, which is off, so no
// download begins and nothing is kept. Before, it began a download for a lookup that was off, and the
// list came into use and was saved.
func TestASNHookOfAnEarlierChangeRunningLateFollowsTheSettingAsItIs(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	// The ASN hook is the first the service registers. Hold the hook of the change that turns the
	// lookup on, at the start, until the change that turns it off has run its own.
	server.service.hooksMu.Lock()
	original := server.service.hooks[0]
	gate := make(chan struct{})
	var held atomic.Bool
	server.service.hooks[0] = func(previous, next *State) {
		if next.Monitor.ASNLookup && held.CompareAndSwap(false, true) {
			<-gate
		}
		original(previous, next)
	}
	server.service.hooksMu.Unlock()

	turnedOn := make(chan struct{})
	go func() {
		defer close(turnedOn)
		body, _ := json.Marshal(map[string]any{"asn_lookup": true})
		request, _ := http.NewRequest("PATCH", server.base+"/server", bytes.NewReader(body))
		if response, err := http.DefaultClient.Do(request); err != nil {
			t.Errorf("turn on: %v", err)
		} else {
			response.Body.Close()
		}
	}()
	waitFor(t, "the change to be saved", func() bool {
		saved := false
		server.service.Read(func(s *State) { saved = s.Monitor.ASNLookup })
		return saved
	})
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": false}, nil)
	close(gate) // now the hook of the first change runs
	<-turnedOn

	time.Sleep(300 * time.Millisecond)
	if source.hits.Load() != 0 {
		t.Fatalf("a download began for a lookup that is off (%d request(s))", source.hits.Load())
	}
	if server.service.asn.table.Load() != nil {
		t.Fatal("a list is in use although the lookup is off")
	}
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile); err == nil {
		t.Fatal("a list is on disk although the lookup is off")
	}
}

func toggleAtOnce(t *testing.T, ends bool) {
	t.Helper()
	serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, nil)
	toggle := func(on bool) {
		body, _ := json.Marshal(map[string]any{"asn_lookup": on})
		request, _ := http.NewRequest("PATCH", server.base+"/server", bytes.NewReader(body))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Errorf("toggle: %v", err)
			return
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("toggle: status %d", response.StatusCode)
		}
	}
	var wg sync.WaitGroup
	for caller := 0; caller < 6; caller++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				toggle(i%2 == 1)
			}
			toggle(ends)
		}()
	}
	wg.Wait()
	idle := func() bool {
		server.service.asn.mu.Lock()
		defer server.service.asn.mu.Unlock()
		return !server.service.asn.busy
	}
	if ends {
		waitFor(t, "the list", func() bool { return server.asnStatus(t).Ready })
		waitFor(t, "the downloads to end", idle)
		if status := server.asnStatus(t); !status.Enabled || status.Error != "" || status.Ranges != 3 {
			t.Fatalf("status while on: %+v", status)
		}
		return
	}
	waitFor(t, "the downloads to end", idle)
	if server.service.asn.table.Load() != nil {
		t.Fatal("a list is in use although the lookup ended off")
	}
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile); err == nil {
		t.Fatal("a list is on disk although the lookup ended off")
	}
	if status := server.asnStatus(t); status.Enabled || status.Error != "" {
		t.Fatalf("status while off: %+v", status)
	}
}

// The list saved by an earlier run is read when the server starts, which takes a while for the real
// one: a lookup turned off meanwhile deleted it, and what was read must not come back into use.
func TestASNLoadDoesNotBringBackAListThatWasDropped(t *testing.T) {
	manager := newASNManager(t.TempDir())
	if err := os.WriteFile(manager.path(), gzipped(t, testASNList), 0o600); err != nil {
		t.Fatal(err)
	}
	reading, proceed := make(chan struct{}), make(chan struct{})
	manager.read = func(string) (*asn.Table, error) {
		close(reading)
		<-proceed
		return mustTable(t), nil
	}
	loaded := make(chan error, 1)
	go func() { loaded <- manager.Load() }()
	<-reading
	manager.Drop() // turned off while the list is being read
	close(proceed)
	if err := <-loaded; err != nil {
		t.Fatal(err)
	}
	if manager.table.Load() != nil || manager.Status(true).Ready {
		t.Fatal("a list read before the lookup was turned off came into use after it")
	}
	if _, err := os.Stat(manager.path()); err == nil {
		t.Fatal("the saved list was kept")
	}
}

// The saved list is read at start without asking anyone; one that cannot be read is removed so
// that the next try downloads a new one.
func TestASNLoadReadsTheSavedList(t *testing.T) {
	source := serveASN(t, nil)
	dir := t.TempDir()
	manager := newASNManager(dir)
	if err := manager.Load(); err != nil || manager.Status(true).Ready {
		t.Fatalf("nothing saved: %v %+v", err, manager.Status(true))
	}
	if err := os.WriteFile(manager.path(), gzipped(t, testASNList), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}
	status := manager.Status(true)
	if !status.Ready || status.Ranges != 3 || time.Since(time.Unix(status.UpdatedUnix, 0)) > time.Minute {
		t.Fatalf("status: %+v", status)
	}
	if info, found := manager.Lookup(netip.MustParseAddr("198.51.100.200")); !found || info.Number != 64498 {
		t.Fatalf("lookup: %+v %v", info, found)
	}

	damaged := newASNManager(t.TempDir())
	if err := os.WriteFile(damaged.path(), []byte("not a gzip file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := damaged.Load(); err == nil {
		t.Fatal("a damaged list was accepted")
	}
	if _, err := os.Stat(damaged.path()); err == nil {
		t.Fatal("a damaged list was kept")
	}
	if source.hits.Load() != 0 {
		t.Fatal("loading asked the network")
	}
}

// A list is downloaded again when there is none, or it is a month old, and not more often than
// every few hours after a failure.
func TestASNDue(t *testing.T) {
	now := time.Now()
	manager := newASNManager(t.TempDir())
	if !manager.Due(now) {
		t.Fatal("not due with no list")
	}
	_, generation, ok := manager.begin(t.Context(), now)
	if !ok {
		t.Fatal("begin")
	}
	if manager.Due(now.Add(48 * time.Hour)) {
		t.Fatal("due while a download is running")
	}
	manager.finish(generation, os.ErrDeadlineExceeded)
	if manager.Due(now.Add(time.Hour)) {
		t.Fatal("due an hour after a failure")
	}
	if !manager.Due(now.Add(asnRetryAfter + time.Minute)) {
		t.Fatal("not due long after a failure with no list")
	}
	manager.table.Store(mustTable(t))
	manager.updated = now
	if manager.Due(now.Add(7 * 24 * time.Hour)) {
		t.Fatal("due with a week-old list")
	}
	if !manager.Due(now.Add(asnRefreshAge + time.Hour)) {
		t.Fatal("not due with a list over a month old")
	}
}

func TestASNLowDiskRefusesTheDownload(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	diskCheck = func(string) (uint64, uint64, bool) { return 100_000_000, 30_000_000_000, true }
	t.Cleanup(func() { diskCheck = diskSpace })
	server := newTestServer(t, nil)
	server.must(t, http.StatusOK, "PATCH", "/server", map[string]any{"asn_lookup": true}, nil)
	waitFor(t, "the try to end", func() bool { s := server.asnStatus(t); return !s.Updating && s.Error != "" })
	if status := server.asnStatus(t); status.Ready || !strings.Contains(status.Error, "free disk space") {
		t.Fatalf("status: %+v", status)
	}
	if source.hits.Load() != 0 {
		t.Fatal("the list was downloaded onto a nearly full disk")
	}
}

// A redirect away from HTTPS is not followed: the list would travel in the clear and could be
// replaced on the way.
func TestASNRefusesARedirectToHTTP(t *testing.T) {
	oldAllow := asnAllowHTTP
	asnAllowHTTP = false
	t.Cleanup(func() { asnAllowHTTP = oldAllow })
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzipped(t, testASNList))
	}))
	t.Cleanup(insecure.Close)
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, insecure.URL+"/list.gz", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	oldURL, oldClient := asnSourceURL, asnHTTPClient
	asnSourceURL = redirector.URL
	asnHTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: redirector.Client().Transport, CheckRedirect: oldClient.CheckRedirect}
	t.Cleanup(func() { asnSourceURL, asnHTTPClient = oldURL, oldClient })

	manager := newASNManager(t.TempDir())
	ctx, generation, _ := manager.begin(t.Context(), time.Now())
	err := manager.download(ctx, generation)
	if err == nil || !strings.Contains(err.Error(), "redirected away from HTTPS") {
		t.Fatalf("download followed a redirect to HTTP: %v", err)
	}
	if manager.Status(true).Ready {
		t.Fatal("a list from a redirect to HTTP is in use")
	}
}

// `ghostlink-server asn ...` and the provider columns of `clients`, on the real API listener.
func TestAdminASNCommands(t *testing.T) {
	source := serveASN(t, gzipped(t, testASNList))
	server := newTestServer(t, func(s *State) { s.API.Port = freeTCPPort(t) })
	alice := server.user(t, map[string]any{"name": "Alice"})
	seenFrom(t, server, alice.ID, "192.0.2.10")
	seenFrom(t, server, alice.ID, "10.9.9.9")
	run := adminCommands(t, server)
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	contains := func(out string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Fatalf("output lacks %q:\n%s", want, out)
			}
		}
	}

	// Off: no provider columns, and the way to get them.
	contains(mustRun("asn", "status"), "off", "asn on")
	out := mustRun("clients")
	contains(out, "ADDRESS", "192.0.2.10", "sudo ghostlink-server asn on")
	if strings.Contains(out, "OPERATOR") {
		t.Fatalf("provider columns while off:\n%s", out)
	}
	contains(mustRun("settings"), "Provider lookup (AS and operator of client addresses): off")

	// On: the command waits for the list and says how it went.
	contains(mustRun("asn", "on"), "Provider lookup is on: 3 address ranges")
	out = mustRun("clients")
	contains(out, "AS", "OPERATOR", "AS64496", "Example Telecom One, Inc.", "10.9.9.9")
	if strings.Contains(out, "asn on") {
		t.Fatalf("the hint is shown with the lookup on:\n%s", out)
	}
	contains(mustRun("settings"), "Provider lookup: on, 3 address ranges from iptoasn.com")
	contains(mustRun("--json", "clients"), `"asn": 64496`, `"operator": "Example Telecom One, Inc."`)
	contains(mustRun("--json", "asn", "status"), `"enabled": true`, `"ready": true`)

	// Update downloads again; a failing source is an error, and the list in use stays.
	contains(mustRun("asn", "update"), "Provider lookup is on")
	if source.hits.Load() != 2 {
		t.Fatalf("downloads: %d, want 2", source.hits.Load())
	}
	source.answer(http.StatusInternalServerError, []byte("no"))
	if out, err := run("asn", "update"); err == nil || !strings.Contains(err.Error(), "could not be downloaded") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("a failing source: %v\n%s", err, out)
	}
	contains(mustRun("clients"), "AS64496")
	contains(mustRun("settings"), "The last update failed")

	// Off deletes the list.
	contains(mustRun("asn", "off"), "off", "deleted")
	contains(mustRun("settings"), "Provider lookup (AS and operator of client addresses): off")
	if _, err := os.Stat(server.store.Dir() + "/" + ASNFile); err == nil {
		t.Fatal("the list is still on disk")
	}
	for _, args := range [][]string{{"asn"}, {"asn", "maybe"}, {"asn", "on", "now"}} {
		if _, err := run(args...); err == nil || !strings.Contains(err.Error(), "usage: asn") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if out, err := run("asn", "update"); err == nil || !strings.Contains(err.Error(), "turn it on first") {
		t.Fatalf("update while off: %v\n%s", err, out)
	}
}
