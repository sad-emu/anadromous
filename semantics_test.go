//go:build linux

package anadromous

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// acceptStream accepts the peer's stream with a timeout.
func acceptStream(t *testing.T, c *Connection) *Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	return s
}

// TestCloseIsFinWriteOnly verifies the BidiPipe pattern: closing a stream
// only FINs the write direction; the read direction keeps working.
func TestCloseIsFinWriteOnly(t *testing.T) {
	client, server := newTestConnectionPair(t)

	cs, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := cs.Write([]byte("ping")); err != nil {
		t.Fatalf("client Write: %v", err)
	}
	if err := cs.Close(); err != nil { // FIN write side only
		t.Fatalf("client Close: %v", err)
	}

	ss := acceptStream(t, server)
	got, err := io.ReadAll(ss)
	if err != nil {
		t.Fatalf("server ReadAll: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("server got %q, want ping", got)
	}

	// The client already Close()d — its read side must still work.
	if _, err := ss.Write([]byte("pong")); err != nil {
		t.Fatalf("server Write after client Close: %v", err)
	}
	if err := ss.Close(); err != nil {
		t.Fatalf("server Close: %v", err)
	}

	back, err := io.ReadAll(cs)
	if err != nil {
		t.Fatalf("client ReadAll after Close: %v", err)
	}
	if string(back) != "pong" {
		t.Fatalf("client got %q, want pong", back)
	}
}

// TestFinBeforeData verifies that a FIN arriving before earlier data frames
// does not cause premature EOF: all data is delivered first.
func TestFinBeforeData(t *testing.T) {
	_, server := newTestConnectionPair(t)

	// FIN at position 2 arrives before either data frame.
	server.recvMu.Lock()
	server.handleStreamFIN(frame{ftype: frameStreamFIN, streamID: 1, seq: 2})
	server.recvMu.Unlock()

	handleDataLocked(server, createTestFrame(1, 1, 1, []byte("world")))
	handleDataLocked(server, createTestFrame(1, 1, 0, []byte("hello")))

	s := serverStream(t, server, 1)
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "helloworld" {
		t.Fatalf("got %q, want helloworld", got)
	}
}

// TestCancelWritePropagates verifies CancelWrite resets the peer's read side.
func TestCancelWritePropagates(t *testing.T) {
	client, server := newTestConnectionPair(t)

	cs, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := cs.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ss := acceptStream(t, server)

	cs.CancelWrite(7)

	ss.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16)
	for {
		_, err = ss.Read(buf)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrStreamReset) {
		t.Fatalf("server Read error = %v, want ErrStreamReset", err)
	}
}

// TestCancelReadPropagates verifies CancelRead cancels the peer's write side.
func TestCancelReadPropagates(t *testing.T) {
	client, server := newTestConnectionPair(t)

	cs, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := cs.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ss := acceptStream(t, server)

	ss.CancelRead(3)

	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err = cs.Write([]byte("y"))
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client Write never failed after peer CancelRead")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !errors.Is(err, ErrStreamReset) {
		t.Fatalf("client Write error = %v, want ErrStreamReset", err)
	}
}

// TestReadDeadlineWhileBlocked verifies a deadline set before blocking fires
// while the Read is blocked, not just on entry.
func TestReadDeadlineWhileBlocked(t *testing.T) {
	client, _ := newTestConnectionPair(t)

	cs, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	cs.SetReadDeadline(time.Now().Add(300 * time.Millisecond))

	start := time.Now()
	buf := make([]byte, 1)
	_, err = cs.Read(buf)
	elapsed := time.Since(start)

	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Fatalf("Read error = %v, want timeout", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("Read returned after %v, want ~300ms", elapsed)
	}
}

// TestStreamGC verifies streams are removed from both connections once both
// directions finish and all frames are acknowledged.
func TestStreamGC(t *testing.T) {
	client, server := newTestConnectionPair(t)

	cs, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := cs.Write([]byte("bye")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cs.Close()

	ss := acceptStream(t, server)
	if _, err := io.ReadAll(ss); err != nil {
		t.Fatalf("server ReadAll: %v", err)
	}
	ss.Close()

	// Client side: drain the server's FIN to finish the read direction.
	if _, err := io.ReadAll(cs); err != nil {
		t.Fatalf("client ReadAll: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		client.streamMu.RLock()
		nc := len(client.streams)
		client.streamMu.RUnlock()
		server.streamMu.RLock()
		ns := len(server.streams)
		server.streamMu.RUnlock()
		if nc == 0 && ns == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("streams not GC'd: client=%d server=%d", nc, ns)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestOpenStreamSyncUnblocks verifies OpenStreamSync waits for a slot.
func TestOpenStreamSyncUnblocks(t *testing.T) {
	ln, err := Listen("127.0.0.1:0", WithMaxStreams(1))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	client, err := Dial(t.Context(), ln.Addr().String(), WithMaxStreams(1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	s1, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := client.OpenStream(t.Context()); err != ErrMaxStreams {
		t.Fatalf("second OpenStream error = %v, want ErrMaxStreams", err)
	}

	opened := make(chan error, 1)
	go func() {
		_, err := client.OpenStreamSync(t.Context())
		opened <- err
	}()

	select {
	case err := <-opened:
		t.Fatalf("OpenStreamSync returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Finish stream 1 in both directions so GC frees the slot. Write once
	// so the peer learns of the stream and can ACK the FIN.
	s1.Write([]byte("z"))
	s1.Close()
	s1.CancelRead(0)

	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("OpenStreamSync: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("OpenStreamSync never unblocked")
	}
}

// TestWindowBlocksAndResumes verifies a writer stalls when the peer stops
// reading (flow control) and resumes when the peer drains.
func TestWindowBlocksAndResumes(t *testing.T) {
	const bufSize = 8 * 1024
	const total = 256 * 1024

	ln, err := Listen("127.0.0.1:0", WithStreamBufferSize(bufSize))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	client, err := Dial(t.Context(), ln.Addr().String(), WithStreamBufferSize(bufSize))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	serverConn, err := ln.Accept(t.Context())
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	cs, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	data := make([]byte, total)
	rand.Read(data)

	done := make(chan error, 1)
	go func() {
		_, werr := cs.Write(data)
		cs.Close()
		done <- werr
	}()

	// The write must stall: total >> stream buffer and nobody is reading.
	select {
	case err := <-done:
		t.Fatalf("Write completed without reader (flow control broken): %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Now drain and verify.
	ss := acceptStream(t, serverConn)
	got, err := io.ReadAll(ss)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if werr := <-done; werr != nil {
		t.Fatalf("Write: %v", werr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("data mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

// TestLargeBidirectionalTransfer pushes several MB both ways at once,
// exercising ring growth, batching, ACKs, and retransmit bookkeeping.
func TestLargeBidirectionalTransfer(t *testing.T) {
	const total = 4 * 1024 * 1024

	ln, err := Listen("127.0.0.1:0", WithStreamBufferSize(1024*1024))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	client, err := Dial(t.Context(), ln.Addr().String(), WithStreamBufferSize(1024*1024))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	serverConn, err := ln.Accept(t.Context())
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	c2s := make([]byte, total)
	s2c := make([]byte, total)
	rand.Read(c2s)
	rand.Read(s2c)

	cs, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 4)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := cs.Write(c2s); err != nil {
			errs <- err
		}
		cs.Close()
	}()

	var clientGot []byte
	wg.Add(1)
	go func() {
		defer wg.Done()
		var rerr error
		clientGot, rerr = io.ReadAll(cs)
		if rerr != nil {
			errs <- rerr
		}
	}()

	ss := acceptStream(t, serverConn)
	var serverGot []byte
	wg.Add(1)
	go func() {
		defer wg.Done()
		var rerr error
		serverGot, rerr = io.ReadAll(ss)
		if rerr != nil {
			errs <- rerr
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := ss.Write(s2c); err != nil {
			errs <- err
		}
		ss.Close()
	}()

	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("transfer error: %v", err)
	default:
	}

	if sha256.Sum256(serverGot) != sha256.Sum256(c2s) {
		t.Fatalf("client->server data corrupted (%d bytes)", len(serverGot))
	}
	if sha256.Sum256(clientGot) != sha256.Sum256(s2c) {
		t.Fatalf("server->client data corrupted (%d bytes)", len(clientGot))
	}
}

func TestWindowResendDiag(t *testing.T) {
	const bufSize = 8 * 1024
	const total = 256 * 1024

	ln, err := Listen("127.0.0.1:0", WithStreamBufferSize(bufSize))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	client, err := Dial(t.Context(), ln.Addr().String(), WithStreamBufferSize(bufSize))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	serverConn, err := ln.Accept(t.Context())
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	cs, _ := client.OpenStream(t.Context())
	data := make([]byte, total)
	done := make(chan struct{})
	go func() { cs.Write(data); cs.Close(); close(done) }()
	ss := acceptStream(t, serverConn)
	start := time.Now()
	got, _ := io.ReadAll(ss)
	<-done
	resends := client.statResends.Load() + serverConn.statResends.Load()
	t.Logf("read %d bytes in %v, resends=%d", len(got), time.Since(start), resends)
	if len(got) != total {
		t.Fatalf("read %d bytes, want %d", len(got), total)
	}
	// Loopback transfers should not lean on the retransmit path at all; a
	// handful of resends is tolerated for scheduling noise.
	if resends > 20 {
		t.Fatalf("excessive retransmissions on loopback: %d", resends)
	}
}
