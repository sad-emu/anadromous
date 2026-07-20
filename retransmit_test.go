//go:build linux

package anadromous

import (
	"testing"
	"time"
)

func TestRetransmitQueueAddDue(t *testing.T) {
	q := newRetransmitQueue(defaultMaxPayloadSize)
	q.add(frameData, 1, 0, []byte("hello"))

	resend := q.due(10 * time.Millisecond)
	if len(resend) != 0 {
		t.Fatalf("expected nothing due immediately, got resend=%d", len(resend))
	}

	time.Sleep(15 * time.Millisecond)
	resend = q.due(10 * time.Millisecond)
	if len(resend) != 1 {
		t.Fatalf("expected 1 due entry, got %d", len(resend))
	}
	if resend[0].streamID != 1 || resend[0].seq != 0 || string(resend[0].data) != "hello" {
		t.Fatalf("unexpected entry: %+v", resend[0])
	}
}

func TestRetransmitQueueAckRemoves(t *testing.T) {
	q := newRetransmitQueue(defaultMaxPayloadSize)
	q.add(frameData, 1, 0, []byte("a"))
	q.add(frameData, 1, 1, []byte("b"))
	q.add(frameData, 2, 0, []byte("c"))

	q.ackMany(1, []uint32{0, 1})

	time.Sleep(5 * time.Millisecond)
	resend := q.due(time.Millisecond)
	if len(resend) != 1 || resend[0].streamID != 2 {
		t.Fatalf("expected only stream 2's entry to remain due, got %+v", resend)
	}
}

func TestRetransmitQueuePurgeStream(t *testing.T) {
	q := newRetransmitQueue(defaultMaxPayloadSize)
	q.add(frameData, 1, 0, []byte("a"))
	q.add(frameData, 2, 0, []byte("b"))

	q.purgeStream(1)

	time.Sleep(5 * time.Millisecond)
	resend := q.due(time.Millisecond)
	if len(resend) != 1 || resend[0].streamID != 2 {
		t.Fatalf("expected only stream 2's entry to remain, got %+v", resend)
	}
}

func TestAckFrameRoundTrip(t *testing.T) {
	buf := make([]byte, defaultMaxDatagramSize)
	seqs := []uint32{1, 2, 5, 9, 100}

	n := encodeAckFrame(buf, 42, seqs)

	f, err := decodeFrame(buf[:n])
	if err != nil {
		t.Fatalf("decodeFrame: %v", err)
	}
	if f.ftype != frameACK {
		t.Fatalf("expected frameACK, got %d", f.ftype)
	}
	if f.streamID != 42 {
		t.Fatalf("expected streamID 42, got %d", f.streamID)
	}

	got, err := decodeAckFrame(f)
	if err != nil {
		t.Fatalf("decodeAckFrame: %v", err)
	}
	if len(got) != len(seqs) {
		t.Fatalf("expected %d seqs, got %d", len(seqs), len(got))
	}
	for i, s := range seqs {
		if got[i] != s {
			t.Fatalf("seq %d: expected %d, got %d", i, s, got[i])
		}
	}
}
