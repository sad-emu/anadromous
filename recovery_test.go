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

func newAckIndexTestStream(entries int) *Stream {
	cfg := defaultConfig()
	c := &Connection{cfg: cfg}
	s := &Stream{
		conn:    c,
		reorder: make(map[uint32][]byte, entries),
	}
	for i := 0; i < entries; i++ {
		seq := uint32(i + 10)
		s.reorder[seq] = []byte{byte(i)}
		s.reorderAckSeqs = append(s.reorderAckSeqs, seq)
	}
	return s
}

func TestAckSnapshotRotatesWithoutMapIteration(t *testing.T) {
	s := newAckIndexTestStream(30)
	_, first, _ := s.ackSnapshot(nil, 10)
	first = append([]uint32(nil), first...)
	_, second, _ := s.ackSnapshot(nil, 10)

	if len(first) != 10 || len(second) != 10 {
		t.Fatalf("snapshot lengths = %d, %d; want 10, 10", len(first), len(second))
	}
	seen := make(map[uint32]bool, 20)
	for _, seq := range first {
		seen[seq] = true
	}
	for _, seq := range second {
		if seen[seq] {
			t.Fatalf("rotating snapshot repeated seq %d before covering remaining entries", seq)
		}
	}
}

func TestAckSnapshotCompactsDrainedIndex(t *testing.T) {
	s := newAckIndexTestStream(200)
	for i := 0; i < 120; i++ {
		delete(s.reorder, uint32(i+10))
		s.reorderAckDead++
	}
	_, got, _ := s.ackSnapshot(nil, 200)
	if len(got) != 80 {
		t.Fatalf("snapshot contains %d live entries, want 80", len(got))
	}
	if len(s.reorderAckSeqs) != 80 || s.reorderAckDead != 0 {
		t.Fatalf("index after compaction: len=%d dead=%d, want 80/0", len(s.reorderAckSeqs), s.reorderAckDead)
	}
}

func BenchmarkAckSnapshotIndexed(b *testing.B) {
	s := newAckIndexTestStream(4096)
	scratch := make([]uint32, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, seqs, _ := s.ackSnapshot(scratch, cap(scratch))
		scratch = seqs[:0]
	}
}
