package anadromous

import (
	"context"
	"testing"
	"time"
)

func newTestConnectionPair(t *testing.T) (*Connection, *Connection) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	addr := ln.Addr().String()
	t.Logf("listener on %s", addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	clientConn, err := Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })

	time.Sleep(50 * time.Millisecond)

	ln.connMu.RLock()
	var serverConn *Connection
	for _, c := range ln.conns {
		serverConn = c
		break
	}
	ln.connMu.RUnlock()

	return clientConn, serverConn
}

func createTestFrame(connID uint32, streamID uint32, seq uint32, payload []byte) frame {
	return frame{
		ftype:    0, // data frame
		streamID: streamID,
		seq:      seq,
		length:   uint32(len(payload)),
		payload:  payload,
	}
}

// handleDataLocked injects a frame the way handleDatagram would: holding
// recvMu, so it doesn't race with the connection's own read loop.
func handleDataLocked(c *Connection, f frame) {
	c.recvMu.Lock()
	c.handleData(f)
	c.recvMu.Unlock()
}

func TestConnectionInOrder(t *testing.T) {
	_, server := newTestConnectionPair(t)
	// send a few in-order packets and verify they are received correctly
	fakeConnectionId := uint32(1)
	server.streams = make(map[uint32]*Stream) // reset streams map for testing
	server.streams[fakeConnectionId] = newStream(fakeConnectionId, server, 1024)
	f1 := createTestFrame(uint32(fakeConnectionId), 1, 0, []byte("hello"))
	f2 := createTestFrame(uint32(fakeConnectionId), 1, 1, []byte("world"))
	f3 := createTestFrame(uint32(fakeConnectionId), 1, 2, []byte("!"))
	handleDataLocked(server, f1)
	handleDataLocked(server, f2)
	handleDataLocked(server, f3)

	server.streams[fakeConnectionId].readMu.Lock()
	if server.streams[fakeConnectionId].readSeq != 3 {
		t.Fatalf("expected readSeq to be 3, got %d", server.streams[fakeConnectionId].readSeq)
	}
	if string(server.streams[fakeConnectionId].readBuf[:server.streams[fakeConnectionId].readLen]) != "helloworld!" {
		t.Fatalf("expected readBuf to contain 'helloworld!', got %q", string(server.streams[fakeConnectionId].readBuf[:server.streams[fakeConnectionId].readLen]))
	}
	server.streams[fakeConnectionId].readMu.Unlock()
}

func TestConnectionOutOfOrder(t *testing.T) {
	_, server := newTestConnectionPair(t)
	// send a few out-of-order packets and verify they are buffered and delivered in order
	fakeConnectionId := uint32(1)
	server.streams = make(map[uint32]*Stream) // reset streams map for testing
	server.streams[fakeConnectionId] = newStream(fakeConnectionId, server, 1024)
	f1 := createTestFrame(uint32(fakeConnectionId), 1, 0, []byte("hello"))
	f2 := createTestFrame(uint32(fakeConnectionId), 1, 1, []byte("world"))
	f3 := createTestFrame(uint32(fakeConnectionId), 1, 2, []byte("!"))

	// Order is reversed
	handleDataLocked(server, f3)
	handleDataLocked(server, f2)
	handleDataLocked(server, f1)

	server.streams[fakeConnectionId].readMu.Lock()
	if server.streams[fakeConnectionId].readSeq != 3 {
		t.Fatalf("expected readSeq to be 3, got %d", server.streams[fakeConnectionId].readSeq)
	}
	if string(server.streams[fakeConnectionId].readBuf[:server.streams[fakeConnectionId].readLen]) != "helloworld!" {
		t.Fatalf("expected readBuf to contain 'helloworld!', got %q", string(server.streams[fakeConnectionId].readBuf[:server.streams[fakeConnectionId].readLen]))
	}
	server.streams[fakeConnectionId].readMu.Unlock()
}

func TestPingPong(t *testing.T) {
	client, _ := newTestConnectionPair(t)
	// send a few out-of-order packets and verify they are buffered and delivered in order
	stream, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	client.SendPing(stream.id, stream.writeSeq)

	// wait a bit for pong to be processed
	for client.GetRtt() < 0 {
		time.Sleep(100 * time.Millisecond)
	}

	if rtt := client.rttMs.Load(); rtt > 3 || rtt == -1 {
		t.Fatalf("expected rttMs to be less than 3, got %d", rtt)
	}
}

func TestPingPongClose(t *testing.T) {
	client, server := newTestConnectionPair(t)
	// send a few out-of-order packets and verify they are buffered and delivered in order
	stream, err := client.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	client.SendPing(stream.id, stream.writeSeq)

	// wait a bit for pong to be processed
	for client.GetRtt() < 0 {
		time.Sleep(100 * time.Millisecond)
	}

	// We just want to make sure there was a round trip here
	if rtt := client.rttMs.Load(); rtt == -1 {
		t.Fatalf("expected rttMs to be anything other than -1, got %d", rtt)
	}

	client.Close()
	time.Sleep(100 * time.Millisecond)

	if !client.closed.Load() {
		t.Fatalf("expected connection to be closed")
	}

	// GoAway should propagate and close the server-side connection too.
	deadline := time.Now().Add(5 * time.Second)
	for !server.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("server connection was not closed by GoAway within 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
