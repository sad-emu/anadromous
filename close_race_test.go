//go:build linux

package anadromous

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClosedConnectionIgnoresLatePeerStream(t *testing.T) {
	cfg := defaultConfig()
	WithFEC(0)(&cfg)
	c := &Connection{
		cfg:           cfg,
		streams:       nil, // the post-Close state that used to panic on assign
		acceptCh:      make(chan *Stream, 1),
		deadStreams:   make(map[uint32]time.Time),
		sReadIdsToAck: make(map[uint32][]uint32),
		sAckDirty:     make(map[uint32]struct{}),
		isClient:      true,
	}
	c.closed.Store(true)

	buf := make([]byte, frameHeaderSize+4)
	n := encodeFrame(buf, frameData, 2, 0, []byte("late")) // even: peer-initiated for a client
	c.handleDatagramAndFlush(buf[:n])
	if len(c.acceptCh) != 0 {
		t.Fatal("closed connection accepted a late peer stream")
	}
	if s, ack := c.streamForFrame(2); s != nil || ack {
		t.Fatalf("closed streamForFrame = (%v, %v), want (nil, false)", s, ack)
	}
}

func TestOpenStreamRejectsClosedNilMap(t *testing.T) {
	cfg := defaultConfig()
	c := &Connection{cfg: cfg, streams: make(map[uint32]*Stream)}

	// Reproduce the check-before-lock interleaving: OpenStream observes an open
	// connection and then blocks on streamMu while Close wins and clears the map.
	c.streamMu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := c.OpenStream(context.Background())
		result <- err
	}()
	time.Sleep(time.Millisecond)
	c.closed.Store(true)
	c.streams = nil
	c.streamMu.Unlock()

	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("OpenStream error = %v, want ErrClosed", err)
	}
}

func TestCloseCancelsBatchWaitingForSharedPacer(t *testing.T) {
	// The handshake is critical and deliberately borrows from this one-byte
	// bucket. A later DATA batch would therefore wait for many seconds unless
	// Close closes closeCh before trying to acquire sendMu for GOAWAY.
	client, _ := newTestConnectionPair(t,
		WithPacingRate(1),
		WithPacingBurstBytes(1),
		WithFEC(0),
	)
	stream, err := client.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("blocked by pacing debt"))
		writeDone <- err
	}()
	// Let Write enter flushSendLocked and wait while holding sendMu.
	time.Sleep(10 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the in-progress batch pacing wait")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("paced writer did not unblock after Close")
	}
}

func TestCloseWhilePeerHasQueuedData(t *testing.T) {
	client, server := newTestConnectionPair(t)
	if server == nil {
		t.Fatal("server connection was not established")
	}
	stream, err := client.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		payload := make([]byte, 64<<10)
		for i := 0; i < 128; i++ {
			if _, err := stream.Write(payload); err != nil {
				return
			}
		}
	}()

	// Give RecvMMsg a chance to hold queued DATA while Close fences ingress.
	time.Sleep(2 * time.Millisecond)
	serverCloseDone := make(chan error, 1)
	go func() { serverCloseDone <- server.Close() }()
	select {
	case err := <-serverCloseDone:
		if err != nil {
			t.Fatalf("server Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server Close deadlocked with queued peer data")
	}
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("peer writer did not unblock after connection close")
	}
	server.streamMu.RLock()
	streams := server.streams
	server.streamMu.RUnlock()
	if streams != nil {
		t.Fatal("closed connection retained its stream map")
	}
}

func TestListenerCloseWaitsForConnectionAlreadyClosing(t *testing.T) {
	ln, err := Listen("127.0.0.1:0", WithFEC(0))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, ln.Addr().String(), WithFEC(0))
	if err != nil {
		ln.Close()
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	server, err := ln.Accept(ctx)
	if err != nil {
		ln.Close()
		t.Fatalf("Accept: %v", err)
	}

	// Hold the ingress fence so the individual connection Close is guaranteed
	// to remain in progress after it has marked the connection closed.
	server.recvMu.Lock()
	connectionCloseDone := make(chan error, 1)
	go func() { connectionCloseDone <- server.Close() }()
	deadline := time.Now().Add(time.Second)
	for !server.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !server.closed.Load() {
		server.recvMu.Unlock()
		ln.Close()
		t.Fatal("connection Close did not start")
	}

	// A closing connection remains in the listener map until its fd and loops
	// are gone, allowing Listener.Close to find it and join the winning Close.
	ln.connMu.RLock()
	retained := false
	for _, conn := range ln.conns {
		if conn == server {
			retained = true
			break
		}
	}
	ln.connMu.RUnlock()
	if !retained {
		server.recvMu.Unlock()
		ln.Close()
		t.Fatal("listener dropped a connection before its teardown completed")
	}

	listenerCloseDone := make(chan error, 1)
	go func() { listenerCloseDone <- ln.Close() }()
	select {
	case err := <-listenerCloseDone:
		server.recvMu.Unlock()
		t.Fatalf("Listener.Close returned before connection teardown: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	server.recvMu.Unlock()

	select {
	case err := <-connectionCloseDone:
		if err != nil {
			t.Fatalf("Connection.Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Connection.Close did not finish after releasing recvMu")
	}
	select {
	case err := <-listenerCloseDone:
		if err != nil {
			t.Fatalf("Listener.Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Listener.Close did not join the closing connection")
	}
}
