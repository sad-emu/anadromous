//go:build linux

package anadromous

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"testing"
	"time"
)

// lossyTestPair builds a connected client/server stream pair through the
// in-process lossy proxy, like lossyBenchPair but for tests: a low-delay
// profile keeps recovery cycles (and so the test) fast while still
// exercising drop, duplication, and jitter-induced reordering.
func lossyTestPair(t *testing.T, profile lossyProfile, opts ...Option) (*Stream, *Stream) {
	t.Helper()
	ln, err := Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	proxy, err := newLossyProxy(ln.Addr().String(), profile)
	if err != nil {
		t.Fatalf("newLossyProxy: %v", err)
	}
	t.Cleanup(proxy.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	client, err := Dial(ctx, proxy.Addr(), opts...)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	server, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	cs, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	// The stream is created lazily on the server when its first frame
	// arrives; kick it and accept.
	if _, err := cs.Write([]byte{0}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ss, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	var one [1]byte
	if _, err := io.ReadFull(ss, one[:]); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	return cs, ss
}

// TestLossyDataIntegrity pushes a pseudo-random payload through the lossy
// proxy and verifies the receiver gets it byte-exact — loss recovery via
// FEC reconstruction, NACK fast-retransmit, and timer retransmit must all
// reproduce the stream, not just its length. Run with and without FEC so
// both recovery configurations are covered; the FEC run also asserts that
// reconstruction actually fired (with 10% loss and group size 8, single
// losses within a group are common by construction).
func TestLossyDataIntegrity(t *testing.T) {
	profile := lossyProfile{
		dropProb: 0.10,
		dupProb:  0.05,
		delay:    5 * time.Millisecond,
		jitter:   2 * time.Millisecond,
		seed:     7,
	}
	const total = 4 << 20

	run := func(t *testing.T, wantFecRecovery bool, opts ...Option) {
		cs, ss := lossyTestPair(t, profile, opts...)

		src := make([]byte, total)
		rand.New(rand.NewSource(1)).Read(src)

		writeErr := make(chan error, 1)
		go func() {
			const chunk = 64 * 1024
			for off := 0; off < len(src); off += chunk {
				end := off + chunk
				if end > len(src) {
					end = len(src)
				}
				if _, err := cs.Write(src[off:end]); err != nil {
					writeErr <- err
					return
				}
			}
			writeErr <- cs.Close()
		}()

		ss.SetReadDeadline(time.Now().Add(60 * time.Second))
		got, err := io.ReadAll(ss)
		if err != nil {
			t.Fatalf("read side: %v (got %d of %d bytes)", err, len(got), total)
		}
		if err := <-writeErr; err != nil {
			t.Fatalf("write side: %v", err)
		}
		if len(got) != total {
			t.Fatalf("length mismatch: got %d, want %d", len(got), total)
		}
		if !bytes.Equal(got, src) {
			for i := range got {
				if got[i] != src[i] {
					t.Fatalf("content mismatch at byte %d of %d", i, total)
				}
			}
		}
		recovered := ss.conn.statFecRecovered.Load()
		t.Logf("resends=%d reorder=%d fec-recovered=%d",
			cs.conn.statResends.Load(), ss.conn.statReorder.Load(), recovered)
		if wantFecRecovery && recovered == 0 {
			t.Fatal("expected at least one FEC reconstruction under 10% loss")
		}
	}

	t.Run("FEC", func(t *testing.T) { run(t, true) })
	t.Run("FEC2D", func(t *testing.T) { run(t, true, WithFEC2D(true)) })
	t.Run("Paced", func(t *testing.T) { run(t, true, WithPacingRate(64<<20)) })
	t.Run("NoFEC", func(t *testing.T) { run(t, false, WithFEC(0)) })
	// The sendmmsg fallback path (kernels/seccomp without io_uring) must
	// deliver the same bytes too.
	t.Run("NoUring", func(t *testing.T) { run(t, true, WithIOUring(false)) })
}
