//go:build linux

package anadromous

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
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

// BenchmarkThroughputPaced10G is the provisioned-line acceptance benchmark.
// The custom metric is carrier-counted output; ordinary B/s remains delivered
// application goodput and is lower when FEC consumes part of the 10 Gbit/s
// wire budget.
func BenchmarkThroughputPaced10G(b *testing.B) {
	cs, ss := benchPair(b,
		WithPacingRate(1_250_000_000),
		WithPacingAccounting(WireAccounting{
			PerDatagramOverhead: 66,
			MinimumDatagramSize: 84,
		}),
	)
	pacer := cs.conn.wirePacer
	before := pacer.Stats()
	benchThroughput(b, cs, ss)
	after := pacer.Stats()
	if elapsed := b.Elapsed(); elapsed > 0 {
		b.ReportMetric(float64(after.WireBytes-before.WireBytes)/elapsed.Seconds(), "wire-B/s")
	}
}

// BenchmarkThroughputPaced10GNoFEC isolates pacing/batching overhead from the
// default parity budget; delivered goodput should approach the wire metric.
func BenchmarkThroughputPaced10GNoFEC(b *testing.B) {
	cs, ss := benchPair(b,
		WithFEC(0),
		WithPacingRate(1_250_000_000),
		WithPacingAccounting(WireAccounting{
			PerDatagramOverhead: 66,
			MinimumDatagramSize: 84,
		}),
	)
	pacer := cs.conn.wirePacer
	before := pacer.Stats()
	benchThroughput(b, cs, ss)
	after := pacer.Stats()
	if elapsed := b.Elapsed(); elapsed > 0 {
		b.ReportMetric(float64(after.WireBytes-before.WireBytes)/elapsed.Seconds(), "wire-B/s")
	}
}

// BenchmarkThroughputPaced10GTwoConnections verifies the aggregate bridge
// case: two independently-stalling connections share one 10 Gbit/s sender
// budget, so either can consume tokens while the other is waiting on protocol
// work without multiplying the provisioned wire rate.
func BenchmarkThroughputPaced10GTwoConnections(b *testing.B) {
	const (
		connections = 2
		chunk       = 64 * 1024
	)
	serverPacer := NewWirePacer(WirePacerConfig{
		RateBytesPerSecond: 1_250_000_000,
		Accounting: WireAccounting{
			PerDatagramOverhead: 66,
			MinimumDatagramSize: 84,
		},
	})
	base := []Option{
		WithMaxDatagramSize(8500),
		WithStreamBufferSize(256 * 1024 * 1024),
	}
	listenOpts := append(append([]Option{}, base...), WithWirePacer(serverPacer))
	ln, err := Listen("127.0.0.1:0", listenOpts...)
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	b.Cleanup(func() { ln.Close() })

	clientPacer := NewWirePacer(WirePacerConfig{
		RateBytesPerSecond: 1_250_000_000,
		Accounting: WireAccounting{
			PerDatagramOverhead: 66,
			MinimumDatagramSize: 84,
		},
	})
	dialOpts := append(append([]Option{}, base...), WithWirePacer(clientPacer))
	writers := make([]*Stream, 0, connections)
	readers := make([]*Stream, 0, connections)
	for i := 0; i < connections; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client, dialErr := Dial(ctx, ln.Addr().String(), dialOpts...)
		cancel()
		if dialErr != nil {
			b.Fatalf("Dial %d: %v", i, dialErr)
		}
		b.Cleanup(func() { client.Close() })
		server, acceptErr := ln.Accept(context.Background())
		if acceptErr != nil {
			b.Fatalf("Accept %d: %v", i, acceptErr)
		}
		writer, openErr := client.OpenStream(context.Background())
		if openErr != nil {
			b.Fatalf("OpenStream %d: %v", i, openErr)
		}
		if _, writeErr := writer.Write([]byte{0}); writeErr != nil {
			b.Fatalf("initial Write %d: %v", i, writeErr)
		}
		reader, streamErr := server.AcceptStream(context.Background())
		if streamErr != nil {
			b.Fatalf("AcceptStream %d: %v", i, streamErr)
		}
		var one [1]byte
		if _, readErr := io.ReadFull(reader, one[:]); readErr != nil {
			b.Fatalf("initial Read %d: %v", i, readErr)
		}
		writers = append(writers, writer)
		readers = append(readers, reader)
	}

	readDone := make(chan error, connections)
	for _, reader := range readers {
		go func(s *Stream) {
			_, copyErr := io.CopyBuffer(io.Discard, s, make([]byte, chunk))
			readDone <- copyErr
		}(reader)
	}
	bufs := make([][]byte, connections)
	for i := range bufs {
		bufs[i] = make([]byte, chunk)
	}

	before := clientPacer.Stats()
	b.SetBytes(connections * chunk)
	b.ResetTimer()
	var writes sync.WaitGroup
	writes.Add(connections)
	for i, writer := range writers {
		go func(s *Stream, buf []byte) {
			defer writes.Done()
			for j := 0; j < b.N; j++ {
				if _, writeErr := s.Write(buf); writeErr != nil {
					b.Errorf("Write: %v", writeErr)
					return
				}
			}
		}(writer, bufs[i])
	}
	writes.Wait()
	b.StopTimer()
	after := clientPacer.Stats()

	for _, writer := range writers {
		writer.Close()
	}
	for range readers {
		if readErr := <-readDone; readErr != nil {
			b.Fatalf("read side: %v", readErr)
		}
	}
	var resends, reorder, recovered int64
	for _, writer := range writers {
		resends += writer.conn.statResends.Load()
	}
	for _, reader := range readers {
		reorder += reader.conn.statReorder.Load()
		recovered += reader.conn.statFecRecovered.Load()
	}
	b.Logf("aggregate sender resends=%d; receiver reorder=%d fec-recovered=%d", resends, reorder, recovered)
	if elapsed := b.Elapsed(); elapsed > 0 {
		b.ReportMetric(float64(after.WireBytes-before.WireBytes)/elapsed.Seconds(), "wire-B/s")
	}
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

// BenchmarkThroughputLossyBigWindow probes whether lossy throughput is
// bound by the in-flight cap (bandwidth-delay ceiling) rather than by
// recovery latency: if this scales with the cap, the path to more lossy
// throughput is provisioning (receive socket buffers + cap), not protocol
// work.
func BenchmarkThroughputLossyBigWindow(b *testing.B) {
	cs, ss := lossyBenchPair(b, WithMaxBytesInFlight(32<<20))
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

// BenchmarkThroughputLossyBigWindow2D adds the second FEC dimension (row
// parity, WithFEC2D) on top of the big-window lossy config — the loss-bound
// regime where multi-loss groups falling back to NACK/RTO recovery are what
// throughput dies of.
func BenchmarkThroughputLossyBigWindow2D(b *testing.B) {
	cs, ss := lossyBenchPair(b, WithMaxBytesInFlight(32<<20), WithFEC2D(true))
	benchThroughput(b, cs, ss)
}

// BenchmarkThroughputLossy2D measures the recommended lossy-link
// configuration: default (RTT-aware, granted-buffer-derived) window plus
// the second FEC dimension for multi-loss recovery.
func BenchmarkThroughputLossy2D(b *testing.B) {
	cs, ss := lossyBenchPair(b, WithFEC2D(true))
	benchThroughput(b, cs, ss)
}

// harshBenchPair is lossyBenchPair at double the loss (harshProfile): the
// regime where multi-loss parity groups dominate and recovery schemes
// separate.
func harshBenchPair(b *testing.B, extra ...Option) (*Stream, *Stream) {
	var proxy *lossyProxy
	return benchPairVia(b, func(serverAddr string) string {
		var err error
		proxy, err = newLossyProxy(serverAddr, harshProfile())
		if err != nil {
			b.Fatalf("newLossyProxy: %v", err)
		}
		b.Cleanup(proxy.Close)
		return proxy.Addr()
	}, extra...)
}

func BenchmarkThroughputHarsh2D(b *testing.B) {
	cs, ss := harshBenchPair(b, WithFEC2D(true))
	benchThroughput(b, cs, ss)
}

func proxyBenchPair(b *testing.B, p lossyProfile, extra ...Option) (*Stream, *Stream) {
	var proxy *lossyProxy
	return benchPairVia(b, func(serverAddr string) string {
		var err error
		proxy, err = newLossyProxy(serverAddr, p)
		if err != nil {
			b.Fatalf("newLossyProxy: %v", err)
		}
		b.Cleanup(proxy.Close)
		return proxy.Addr()
	}, extra...)
}

// Probe: proxy ceiling with NO impairment beyond delay (window/BDP bound).
func BenchmarkProxyCeiling50ms(b *testing.B) {
	cs, ss := proxyBenchPair(b, lossyProfile{delay: 50 * time.Millisecond, seed: 1})
	benchThroughput(b, cs, ss)
}

func BenchmarkProxyPaced1G(b *testing.B) {
	cs, ss := proxyBenchPair(b, lossyProfile{delay: 50 * time.Millisecond, seed: 1},
		WithPacingRate(1000<<20))
	benchThroughput(b, cs, ss)
}

func BenchmarkLossyPaced(b *testing.B) {
	cs, ss := lossyBenchPair(b, WithPacingRate(1000<<20))
	benchThroughput(b, cs, ss)
}
