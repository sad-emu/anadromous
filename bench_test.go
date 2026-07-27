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
	return benchPairVia(b, nil, extra...)
}

// benchPairVia is benchPair but dials through an optional address rewriter
// (used to interpose the lossy proxy between the endpoints).
func benchPairVia(b *testing.B, via func(serverAddr string) string, extra ...Option) (*Stream, *Stream) {
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

	dialAddr := ln.Addr().String()
	if via != nil {
		dialAddr = via(dialAddr)
	}
	client, err := Dial(b.Context(), dialAddr, opts...)
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

// BenchmarkThroughputLossy pushes bulk data through the in-process hostile
// network simulator (netem-like: 50ms±10ms each way, 10% loss, 5% dup —
// see lossy_test.go), measuring poor-conditions throughput reproducibly and
// without root. Expect two orders of magnitude below the clean benchmarks:
// the profile's ~100ms RTT and loss-recovery latency dominate.
func BenchmarkThroughputLossy(b *testing.B) {
	cs, ss := lossyBenchPair(b)
	benchThroughput(b, cs, ss)
}

// BenchmarkThroughputLossyNoFEC isolates FEC's contribution under loss.
func BenchmarkThroughputLossyNoFEC(b *testing.B) {
	cs, ss := lossyBenchPair(b, WithFEC(0))
	benchThroughput(b, cs, ss)
}

func lossyBenchPair(b *testing.B, extra ...Option) (*Stream, *Stream) {
	var proxy *lossyProxy
	return benchPairVia(b, func(serverAddr string) string {
		var err error
		proxy, err = newLossyProxy(serverAddr, netemLikeProfile())
		if err != nil {
			b.Fatalf("newLossyProxy: %v", err)
		}
		b.Cleanup(proxy.Close)
		return proxy.Addr()
	}, extra...)
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
	b.Logf("sender resends=%d; receiver reorder=%d fec-recovered=%d",
		cs.conn.statResends.Load(), ss.conn.statReorder.Load(),
		ss.conn.statFecRecovered.Load())
}
