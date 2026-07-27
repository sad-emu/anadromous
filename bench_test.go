//go:build linux

package anadromous

import (
	"io"
	"testing"
)

// benchPair builds a connected client/server pair on loopback with settings
// mirroring the salmon-cannon netem test (8500-byte datagrams, large stream
// buffer), without any netem in the way — this measures the protocol's clean
// -network ceiling and is the harness for CPU/alloc profiling.
func benchPair(b *testing.B, extra ...Option) (*Stream, *Stream) {
	b.Helper()
	opts := append([]Option{
		WithMaxDatagramSize(8500),
		WithStreamBufferSize(256 * 1024 * 1024),
	}, extra...)
	ln, err := Listen("127.0.0.1:0", opts...)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	b.Cleanup(func() { ln.Close() })

	client, err := Dial(b.Context(), ln.Addr().String(), opts...)
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	b.Cleanup(func() { client.Close() })

	server, err := ln.Accept(b.Context())
	if err != nil {
		b.Fatalf("Accept: %v", err)
	}

	cs, err := client.OpenStream(b.Context())
	if err != nil {
		b.Fatalf("OpenStream: %v", err)
	}
	// The stream is created lazily on the server when its first frame
	// arrives; kick it and accept.
	if _, err := cs.Write([]byte{0}); err != nil {
		b.Fatalf("Write: %v", err)
	}
	ss, err := server.AcceptStream(b.Context())
	if err != nil {
		b.Fatalf("AcceptStream: %v", err)
	}
	var one [1]byte
	if _, err := io.ReadFull(ss, one[:]); err != nil {
		b.Fatalf("ReadFull: %v", err)
	}
	return cs, ss
}

// BenchmarkThroughput pushes bulk data client->server over loopback and
// reports bytes/sec (see the B/s figure, or MB/s = B/s / 1e6).
func BenchmarkThroughput(b *testing.B) {
	cs, ss := benchPair(b)
	benchThroughput(b, cs, ss)
}

// BenchmarkThroughputGSO is the same with WithGSO(true), for measuring the
// send-side packing path. Note: on an unpaced low-RTT loopback this is
// expected to LOSE to the default — see the WithGSO doc comment.
func BenchmarkThroughputGSO(b *testing.B) {
	cs, ss := benchPair(b, WithGSO(true))
	benchThroughput(b, cs, ss)
}

func benchThroughput(b *testing.B, cs, ss *Stream) {

	const chunk = 64 * 1024
	buf := make([]byte, chunk)

	done := make(chan error, 1)
	go func() {
		_, err := io.CopyBuffer(io.Discard, ss, make([]byte, chunk))
		done <- err
	}()

	b.SetBytes(chunk)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cs.Write(buf); err != nil {
			b.Fatalf("Write: %v", err)
		}
	}
	b.StopTimer()

	cs.Close()
	if err := <-done; err != nil {
		b.Fatalf("read side: %v", err)
	}
	b.Logf("sender resends=%d; receiver reorder=%d",
		cs.conn.statResends.Load(), ss.conn.statReorder.Load())
}
