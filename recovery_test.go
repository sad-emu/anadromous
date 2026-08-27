//go:build linux

package anadromous

import (
	"testing"
	"time"
)

func TestRetransmitTimeoutOptionsAreIndependent(t *testing.T) {
	cfg := defaultConfig()
	WithRetransmitTimeout(750 * time.Millisecond)(&cfg)
	WithMinRetransmitTimeout(90 * time.Millisecond)(&cfg)
	if cfg.retransmitTmout != 750*time.Millisecond {
		t.Fatalf("initial RTO = %v, want 750ms", cfg.retransmitTmout)
	}
	if cfg.minRetransmitTmout != 90*time.Millisecond {
		t.Fatalf("minimum RTO = %v, want 90ms", cfg.minRetransmitTmout)
	}
}

func TestMaxDatagramMustFitSelectiveAck(t *testing.T) {
	cfg := defaultConfig()
	original := cfg.maxPayload
	WithMaxDatagramSize(frameHeaderSize + 11)(&cfg)
	if cfg.maxPayload != original {
		t.Fatalf("undersized datagram changed max payload to %d, want %d", cfg.maxPayload, original)
	}
	WithMaxDatagramSize(frameHeaderSize + 12)(&cfg)
	if got := maxAckSeqsPerFrame(cfg.maxPayload); got != 1 {
		t.Fatalf("minimum accepted datagram fits %d selective ACK seqs, want 1", got)
	}
}

func TestAdaptiveRTOUsesSeparateFloor(t *testing.T) {
	cfg := defaultConfig()
	c := &Connection{cfg: cfg}
	c.rtoNs.Store(int64(cfg.retransmitTmout))
	if got := c.currentRTO(); got != 300*time.Millisecond {
		t.Fatalf("initial RTO = %v, want 300ms", got)
	}

	// The first 20ms sample computes 20 + 4*(20/2) = 60ms, which is
	// clamped by the independent 150ms adaptive floor rather than the 300ms
	// initial timeout.
	c.updateRTO(20 * time.Millisecond)
	if got := c.currentRTO(); got != 150*time.Millisecond {
		t.Fatalf("adaptive RTO = %v, want 150ms floor", got)
	}
}

func TestNackVolleySpansExistingFrames(t *testing.T) {
	seqs := make([]uint32, maxNackSeqsPerFire)
	for i := range seqs {
		seqs[i] = uint32(i)
	}
	maxPerFrame := maxAckSeqsPerFrame(defaultMaxPayloadSize)
	frames, covered := 0, 0
	err := forEachSeqListChunk(seqs, defaultMaxPayloadSize, func(chunk []uint32) error {
		frames++
		covered += len(chunk)
		if len(chunk) > maxPerFrame {
			t.Fatalf("chunk has %d seqs, wire maximum is %d", len(chunk), maxPerFrame)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if frames <= 1 || covered != maxNackSeqsPerFire {
		t.Fatalf("volley used %d frames and covered %d seqs, want multiple frames covering %d", frames, covered, maxNackSeqsPerFire)
	}
}

func newAckPendingTestStream(entries int) *Stream {
	cfg := defaultConfig()
	c := &Connection{cfg: cfg}
	s := &Stream{
		conn:    c,
		reorder: make(map[uint32][]byte, entries),
	}
	for i := 0; i < entries; i++ {
		seq := uint32(i + 10)
		s.reorder[seq] = []byte{byte(i)}
		s.ackPendingSeqs = append(s.ackPendingSeqs, seq)
	}
	return s
}

func TestAckSnapshotReplaysOneDeltaWithoutRescanning(t *testing.T) {
	s := newAckPendingTestStream(30)
	_, first, _ := s.ackSnapshot(nil)
	first = append([]uint32(nil), first...)
	_, replay, _ := s.ackSnapshot(nil)
	_, expired, _ := s.ackSnapshot(nil)

	if len(first) != 30 || len(replay) != 30 || len(expired) != 0 {
		t.Fatalf("snapshot lengths = %d, %d, %d; want 30, 30, 0", len(first), len(replay), len(expired))
	}
	for i, seq := range first {
		if want := uint32(i + 10); seq != want {
			t.Fatalf("first snapshot seq[%d] = %d, want %d", i, seq, want)
		}
	}
	for i, seq := range replay {
		if want := uint32(i + 10); seq != want {
			t.Fatalf("replay snapshot seq[%d] = %d, want %d", i, seq, want)
		}
	}
	if len(s.ackPendingSeqs) != 0 || len(s.ackReplaySeqs) != 0 {
		t.Fatalf("pending/replay seqs = %d/%d, want 0/0", len(s.ackPendingSeqs), len(s.ackReplaySeqs))
	}
}

func TestAckSnapshotDropsPendingAndReplayCoveredByCumulative(t *testing.T) {
	s := newAckPendingTestStream(20)
	s.readSeq = 20
	_, got, _ := s.ackSnapshot(nil)
	if len(got) != 10 {
		t.Fatalf("snapshot contains %d selective seqs, want 10", len(got))
	}
	for i, seq := range got {
		if want := uint32(i + 20); seq != want {
			t.Fatalf("snapshot seq[%d] = %d, want %d", i, seq, want)
		}
	}
	s.readSeq = 25
	_, replay, _ := s.ackSnapshot(nil)
	if len(replay) != 5 {
		t.Fatalf("replay contains %d selective seqs, want 5", len(replay))
	}
	for i, seq := range replay {
		if want := uint32(i + 25); seq != want {
			t.Fatalf("replay seq[%d] = %d, want %d", i, seq, want)
		}
	}
}

func TestDuplicateOutOfOrderFrameQueuesFreshSelectiveAck(t *testing.T) {
	s := newAckPendingTestStream(1)
	_, first, _ := s.ackSnapshot(nil)
	if len(first) != 1 || first[0] != 10 {
		t.Fatalf("first snapshot = %v, want [10]", first)
	}
	// Expire the one-generation replay, then model the sender retrying the
	// frame because both ACK copies were lost.
	s.ackSnapshot(nil)
	if !s.deliverLocked(10, []byte{0}, false) {
		t.Fatal("duplicate buffered frame was not acknowledged")
	}
	_, retry, _ := s.ackSnapshot(nil)
	if len(retry) != 1 || retry[0] != 10 {
		t.Fatalf("retry snapshot = %v, want [10]", retry)
	}
}

func BenchmarkAckSnapshotPending(b *testing.B) {
	s := newAckPendingTestStream(0)
	scratch := make([]uint32, 0, 2048)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(s.ackPendingSeqs) == 0 {
			for seq := uint32(10); seq < 1034; seq++ {
				s.ackPendingSeqs = append(s.ackPendingSeqs, seq)
			}
		}
		_, seqs, _ := s.ackSnapshot(scratch)
		scratch = seqs[:0]
	}
}
