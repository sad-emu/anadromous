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
	defer ln.Close()

	addr := ln.Addr().String()
	t.Logf("listener on %s", addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clientConn, err := Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clientConn.Close()

	time.Sleep(50 * time.Millisecond)
	firstKey := ""
	for key := range ln.conns {
		firstKey = key
		break
	}

	serverConn := ln.conns[firstKey]
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

func TestConnectionInOrder(t *testing.T) {
	_, server := newTestConnectionPair(t)
	// send a few in-order packets and verify they are received correctly
	fakeConnectionId := uint32(1)
	server.streams = make(map[uint32]*Stream) // reset streams map for testing
	server.streams[fakeConnectionId] = newStream(fakeConnectionId, server, 1024)
	f1 := createTestFrame(uint32(fakeConnectionId), 1, 0, []byte("hello"))
	f2 := createTestFrame(uint32(fakeConnectionId), 1, 1, []byte("world"))
	f3 := createTestFrame(uint32(fakeConnectionId), 1, 2, []byte("!"))
	server.handleData(f1)
	server.handleData(f2)
	server.handleData(f3)

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
	server.handleData(f3)
	server.handleData(f2)
	server.handleData(f1)

	server.streams[fakeConnectionId].readMu.Lock()
	if server.streams[fakeConnectionId].readSeq != 3 {
		t.Fatalf("expected readSeq to be 3, got %d", server.streams[fakeConnectionId].readSeq)
	}
	if string(server.streams[fakeConnectionId].readBuf[:server.streams[fakeConnectionId].readLen]) != "helloworld!" {
		t.Fatalf("expected readBuf to contain 'helloworld!', got %q", string(server.streams[fakeConnectionId].readBuf[:server.streams[fakeConnectionId].readLen]))
	}
	server.streams[fakeConnectionId].readMu.Unlock()
}
