// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package server

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ghostlink.local/outline-worker/internal/asn"
)

// The provider lookup shows, beside the address a client connects from, the autonomous system
// (AS) and the operator that address belongs to ("AS64496 Example Telecom"). It is off until the
// owner turns it on, because turning it on makes this server download a public list from a third
// party. Only the list is downloaded: no address of a client, and nothing about one, is sent to
// anyone. The lookup itself is a search in memory.

// ASNFile is the downloaded list, in the state directory.
const ASNFile = "asn.tsv.gz"

const (
	// asnMaxDownload and asnMaxExpanded bound what a download may take: the real list is 11 MB
	// compressed and 55 MB expanded.
	asnMaxDownload = 64 << 20
	// asnRefreshAge is how old the list may get before it is downloaded again; the registries'
	// assignments change slowly. asnRetryAfter is the wait after a failed try.
	asnRefreshAge = 30 * 24 * time.Hour
	asnRetryAfter = 6 * time.Hour
	// asnMinFree is the free disk space a download needs (the list is kept, and a copy is kept
	// next to it while it is checked).
	asnMinFree = 200_000_000
	// ASNSource names where the list comes from, as shown to the owner.
	ASNSource = "iptoasn.com"
)

// The list is the combined IPv4 and IPv6 data of iptoasn.com, which it releases to the public
// domain (PDDL 1.0) and updates hourly. Tests point these elsewhere.
var (
	asnMaxExpanded int64 = 256 << 20
	asnSourceURL         = "https://iptoasn.com/data/ip2asn-combined.tsv.gz"
	asnMinRanges         = 100_000 // a list with fewer ranges is cut off or not the list
	asnHTTPClient        = &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Scheme != "https" && !asnAllowHTTP {
			return errors.New("the download was redirected away from HTTPS")
		}
		return nil
	}}
	asnAllowHTTP = false
)

// asnManager keeps the list in memory and downloads it when asked.
type asnManager struct {
	dir   string
	table atomic.Pointer[asn.Table]
	// read parses a saved or downloaded list (tests replace it).
	read func(path string) (*asn.Table, error)

	mu      sync.Mutex
	updated time.Time // when the list in memory was downloaded
	failed  string    // why the last try failed ("" after a success)
	tried   time.Time // when the last try began
	// busy is set from the start of a download to the end of the goroutine that runs it, also
	// when the download was ended because the lookup was turned off: that stops within moments.
	busy bool
	// generation counts the times the lookup was turned off: a download, or the reading of the
	// saved list, that began before is not put in use.
	generation int
	// cancel ends the download that is running (see Drop).
	cancel context.CancelFunc
}

func newASNManager(dir string) *asnManager { return &asnManager{dir: dir, read: readASNFile} }

func (m *asnManager) path() string { return filepath.Join(m.dir, ASNFile) }

// Lookup finds the AS of an address; nothing is found while no list is loaded.
func (m *asnManager) Lookup(addr netip.Addr) (asn.Info, bool) { return m.table.Load().Lookup(addr) }

// asnStatus is what the owner sees of the lookup.
type asnStatus struct {
	Enabled bool `json:"enabled"`
	// Ready says a list is loaded, so addresses are being looked up.
	Ready       bool   `json:"ready"`
	Updating    bool   `json:"updating"`
	Ranges      int    `json:"ranges"`
	UpdatedUnix int64  `json:"updated_unix,omitempty"`
	Error       string `json:"error,omitempty"`
	Source      string `json:"source"`
}

func (m *asnManager) Status(enabled bool) asnStatus {
	if !enabled {
		return asnStatus{Source: ASNSource}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status := asnStatus{Enabled: enabled, Updating: m.busy, Error: m.failed, Source: ASNSource}
	if table := m.table.Load(); table != nil {
		status.Ready, status.Ranges = true, table.Len()
		status.UpdatedUnix = m.updated.Unix()
	}
	return status
}

// Load reads the list saved by an earlier download, if there is one. A damaged file is removed
// so it is downloaded again.
func (m *asnManager) Load() error {
	m.mu.Lock()
	generation := m.generation
	m.mu.Unlock()
	info, err := os.Stat(m.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	table, err := m.read(m.path())
	m.mu.Lock()
	defer m.mu.Unlock()
	// Reading takes a while. If the lookup was turned off meanwhile, which deleted the list, what
	// was read is not used; nor is it when a download has put a list in use since.
	if m.generation != generation || m.table.Load() != nil {
		return nil
	}
	if err != nil {
		_ = os.Remove(m.path())
		return fmt.Errorf("the saved list could not be read and was removed: %w", err)
	}
	m.table.Store(table)
	m.updated = info.ModTime()
	return nil
}

// readASNFile parses a downloaded list (gzip) with limits on what it may expand to.
func readASNFile(path string) (*asn.Table, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(io.LimitReader(file, asnMaxDownload))
	if err != nil {
		return nil, fmt.Errorf("not a gzip file: %w", err)
	}
	defer unzipped.Close()
	// One byte more than allowed, so a list that is too large is refused instead of cut short.
	limited := &io.LimitedReader{R: unzipped, N: asnMaxExpanded + 1}
	table, err := asn.Parse(limited)
	if err != nil {
		return nil, err
	}
	if limited.N <= 0 {
		return nil, fmt.Errorf("the list expands to more than %d MB", asnMaxExpanded>>20)
	}
	if table.Len() < asnMinRanges {
		return nil, fmt.Errorf("the list has only %d ranges, so it is cut off or not the list", table.Len())
	}
	return table, nil
}

// Due says whether the list should be downloaded now: none yet, or an old one, and not tried
// lately.
func (m *asnManager) Due(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy || now.Sub(m.tried) < asnRetryAfter {
		return false
	}
	return m.table.Load() == nil || now.Sub(m.updated) > asnRefreshAge
}

// begin marks a download as running. It returns the context the download runs under, which ends
// when the lookup is turned off (see Drop) or parent ends, and the generation the download
// belongs to. It reports false when a download is running already.
func (m *asnManager) begin(parent context.Context, now time.Time) (context.Context, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy {
		return nil, 0, false
	}
	var ctx context.Context
	ctx, m.cancel = context.WithCancel(parent)
	m.busy, m.tried = true, now
	return ctx, m.generation, true
}

// finish records how a download of the given generation went, and reports whether the download
// still counts: it does not when the lookup was turned off while it ran, and then it says nothing
// about the lookup, which may be on again by now. The list in use is kept when a download failed.
func (m *asnManager) finish(generation int, err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = false
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if generation != m.generation {
		return false
	}
	if err != nil {
		m.failed = err.Error()
	} else {
		m.failed = ""
	}
	return true
}

func (m *asnManager) download(ctx context.Context, generation int) error {
	if free, _, ok := diskCheck(m.dir); ok && free < asnMinFree {
		return fmt.Errorf("not enough free disk space for the list (%s free, %s needed)", dataSize(free), dataSize(asnMinFree))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asnSourceURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "ghostlink-server (provider lookup; downloads the list at most once a month)")
	response, err := asnHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("could not download the list from %s: %w", ASNSource, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", ASNSource, response.Status)
	}
	pending := m.path() + ".pending"
	file, err := os.OpenFile(pending, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(pending)
	written, err := io.Copy(file, io.LimitReader(response.Body, asnMaxDownload+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("the download from %s stopped: %w", ASNSource, err)
	}
	if written > asnMaxDownload {
		return fmt.Errorf("the list is larger than %d MB, so it was refused", asnMaxDownload>>20)
	}
	table, err := m.read(pending)
	if err != nil {
		return fmt.Errorf("the list from %s was refused: %w", ASNSource, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Under the lock, so that a lookup turned off at this moment either deletes the list this
	// puts in place, or is seen here.
	if m.generation != generation {
		return errors.New("the lookup was turned off while the list was downloading")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(pending, m.path()); err != nil {
		return err
	}
	m.table.Store(table)
	m.updated = time.Now()
	return nil
}

// Drop forgets the list and deletes it, when the lookup is turned off. A download that is running
// is ended, and what it found is not put in use.
func (m *asnManager) Drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.table.Store(nil)
	m.updated, m.failed, m.tried = time.Time{}, "", time.Time{}
	m.generation++
	_ = os.Remove(m.path())
	_ = os.Remove(m.path() + ".pending")
}

// beginASN downloads the list in the background, unless a download is running or the service is
// closing. Called with the store locked and the lookup on, so the setting cannot change before the
// download has begun, and Close, which takes the same lock, cannot start waiting for the downloads
// in between.
func (s *Service) beginASN(now time.Time) {
	if s.asnCtx.Err() != nil {
		return
	}
	ctx, generation, ok := s.asn.begin(s.asnCtx, now)
	if !ok {
		return
	}
	failedBefore := s.asn.Status(true).Error != ""
	s.asnWG.Add(1)
	go func() {
		defer s.asnWG.Done()
		err := s.asn.download(ctx, generation)
		switch current := s.asn.finish(generation, err); {
		case !current:
			// Turned off while it ran. If it was turned on again meanwhile, its own download was
			// refused because this one still ran: it is due now.
			s.refreshASNIfDue(time.Now())
		case err != nil && s.asnCtx.Err() == nil:
			s.report("provider lookup", err)
		case err == nil && failedBefore:
			s.report("provider lookup", nil)
		}
	}()
}

// RefreshASN downloads the list in the background, now, whatever its age (the owner asked for it).
// It does nothing when a download is running, and reports false, doing nothing, when the lookup is
// off. A try that fails is reported and tried again later (refreshASNIfDue).
func (s *Service) RefreshASN(now time.Time) bool {
	s.store.Lock()
	defer s.store.Unlock()
	if !s.store.State.Monitor.ASNLookup {
		return false
	}
	s.beginASN(now)
	return true
}

// refreshASNIfDue downloads the list in the background when the lookup is on and the list is
// missing or old, and the last try was not lately. Called at start and every hour.
func (s *Service) refreshASNIfDue(now time.Time) {
	s.store.Lock()
	defer s.store.Unlock()
	if s.store.State.Monitor.ASNLookup && s.asn.Due(now) {
		s.beginASN(now)
	}
}

// syncASN makes the lookup follow the setting as it is now: on downloads the list, off forgets it
// and ends a download that is running. It reads the setting itself and acts before the setting can
// change again, instead of acting on the change that called it: changes made at once run their
// hooks in any order, and the hook of an older one must not undo a newer one.
func (s *Service) syncASN(now time.Time) {
	s.store.Lock()
	defer s.store.Unlock()
	if s.store.State.Monitor.ASNLookup {
		s.beginASN(now)
	} else {
		s.asn.Drop()
	}
}

// annotateASN adds the provider of each address to the clients listed, when the lookup is on.
func (s *Service) annotateASN(clients []clientView) asnStatus {
	enabled := false
	s.Read(func(state *State) { enabled = state.Monitor.ASNLookup })
	status := s.asn.Status(enabled)
	if !enabled || !status.Ready {
		return status
	}
	for i := range clients {
		if addr, err := netip.ParseAddr(clients[i].IP); err == nil {
			if info, found := s.asn.Lookup(addr); found {
				clients[i].ASN, clients[i].Operator = info.Number, info.Operator
			}
		}
	}
	return status
}
