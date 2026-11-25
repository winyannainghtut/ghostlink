// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2026 Ghostlink contributors

package awg

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
)

// testDialer dials nothing: it fails with err, or hands out one end of a pipe.
type testDialer struct{ err error }

func (d testDialer) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	if d.err != nil {
		return nil, d.err
	}
	local, _ := net.Pipe()
	return local, nil
}

func (testDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("not used")
}

var testServer = netip.MustParseAddrPort("192.0.2.1:51820")

// WireGuard holds its network lock while it calls Send, and the lock is what stopping or
// restarting the device waits for. A bind that was closed must say so at once: waiting a second
// first would hold up the device's own shutdown, once for every packet still to be sent.
func TestSendOnAClosedBindFailsAtOnce(t *testing.T) {
	bind := newClientBind(context.Background(), logger.NOP(), testDialer{}, testServer)
	if _, _, err := bind.Open(0); err != nil {
		t.Fatal(err)
	}
	if err := bind.Close(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := bind.Send([][]byte{[]byte("packet")}, remoteEndpoint(testServer))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Send on a closed bind: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("Send on a closed bind took %v", elapsed)
	}
}

// A dial that fails is still not tried again for a second: with the network down, the sender
// would otherwise spin.
func TestSendAfterAFailedDialWaits(t *testing.T) {
	broken := errors.New("network is unreachable")
	bind := newClientBind(context.Background(), logger.NOP(), testDialer{err: broken}, testServer)
	if _, _, err := bind.Open(0); err != nil {
		t.Fatal(err)
	}
	defer bind.Close()
	started := time.Now()
	err := bind.Send([][]byte{[]byte("packet")}, remoteEndpoint(testServer))
	if !errors.Is(err, broken) {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("Send after a failed dial returned after %v: nothing slows a sender down while the network is out", elapsed)
	}
}

// Open makes the next generation of the state Close left (done, the context to dial with). The
// other readers of it hold connAccess, so Open takes it too: it waits for a connect in progress
// instead of replacing the state under it. After it, the bind connects again.
func TestOpenReplacesTheShutdownStateUnderTheLock(t *testing.T) {
	bind := newClientBind(context.Background(), logger.NOP(), testDialer{}, testServer)
	if _, _, err := bind.Open(0); err != nil {
		t.Fatal(err)
	}
	if err := bind.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := bind.connect(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("connect on a closed bind: %v", err)
	}

	bind.connAccess.Lock() // a connect (or Send) is in progress
	opened := make(chan struct{})
	go func() {
		_, _, _ = bind.Open(0)
		close(opened)
	}()
	select {
	case <-opened:
		bind.connAccess.Unlock()
		t.Fatal("Open replaced the shutdown state while a connect held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	bind.connAccess.Unlock()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not finish once the lock was free")
	}
	conn, err := bind.connect()
	if err != nil || conn == nil {
		t.Fatalf("the reopened bind does not connect: %v", err)
	}
	if err := bind.Close(); err != nil {
		t.Fatal(err)
	}
}
