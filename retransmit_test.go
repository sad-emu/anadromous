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
	if resend[0].streamID != 1 || resend[0].seq != 0 {
		t.Fatalf("unexpected entry: %+v", resend[0])
	}
	e, ok := q.takeForResend(resend[0])
	if !ok || string(e.data) != "hello" {
		t.Fatalf("takeForResend: ok=%v entry=%+v", ok, e)
	}
	q.releaseResendCopy(e.data)
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
	const cum = uint32(77)

	n := encodeAckFrame(buf, 42, cum, seqs)

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

	gotCum, got, err := decodeSeqListFrame(f, nil)
	if err != nil {
		t.Fatalf("decodeSeqListFrame: %v", err)
	}
	if gotCum != cum {
		t.Fatalf("expected cumulative %d, got %d", cum, gotCum)
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

func TestRetransmitQueueAckCumulative(t *testing.T) {
	q := newRetransmitQueue(defaultMaxPayloadSize)
	q.add(frameData, 1, 0, []byte("aa"))
	q.add(frameData, 1, 1, []byte("bb"))
	q.add(frameData, 1, 2, []byte("cc"))
	q.add(frameData, 2, 0, []byte("dd"))

	freed, _ := q.ackCumulative(1, 2) // covers seqs 0 and 1 of stream 1
	if freed != 4 {
		t.Fatalf("expected 4 freed bytes, got %d", freed)
	}

	// Repeat and stale cumulative values are no-ops.
	if freed, _ := q.ackCumulative(1, 2); freed != 0 {
		t.Fatalf("expected repeat cumulative to free 0, got %d", freed)
	}
	if freed, _ := q.ackCumulative(1, 1); freed != 0 {
		t.Fatalf("expected stale cumulative to free 0, got %d", freed)
	}

	// An implausibly huge jump is treated as corruption and ignored.
	if freed, _ := q.ackCumulative(1, 1<<30); freed != 0 {
		t.Fatalf("expected corrupt cumulative to free 0, got %d", freed)
	}

	// Streams 1 (seq 2) and 2 (seq 0) remain.
	time.Sleep(2 * time.Millisecond)
	resend := q.due(time.Millisecond)
	if len(resend) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d: %+v", len(resend), resend)
	}
}

func TestRetransmitRequestDeduplicatesAndRevalidates(t *testing.T) {
	q := newRetransmitQueue(defaultMaxPayloadSize)
	q.add(frameData, 7, 11, []byte("payload"))

	req, ok := q.requestForResend(7, 11, 0)
	if !ok {
		t.Fatal("expected first request to reserve entry")
	}
	if _, ok := q.requestForResend(7, 11, 0); ok {
		t.Fatal("duplicate request must be suppressed while queued")
	}
	if typ, n, ok := q.peekForResend(req); !ok || typ != frameData || n != len("payload") {
		t.Fatalf("unexpected peek: type=%d len=%d ok=%v", typ, n, ok)
	}

	// ACK can win while the request waits behind pacing. takeForResend must
	// revalidate the key rather than retaining the old arena pointer.
	q.ackOne(7, 11)
	if _, ok := q.takeForResend(req); ok {
		t.Fatal("ACKed request must be canceled before send")
	}
}

func TestRetransmitTakeCopiesPayload(t *testing.T) {
	q := newRetransmitQueue(defaultMaxPayloadSize)
	q.add(frameData, 3, 4, []byte("before"))
	req, ok := q.requestForResend(3, 4, 0)
	if !ok {
		t.Fatal("requestForResend failed")
	}
	e, ok := q.takeForResend(req)
	if !ok {
		t.Fatal("takeForResend failed")
	}
	q.ackOne(3, 4)
	q.drainPendingFree()
	if got := string(e.data); got != "before" {
		t.Fatalf("worker copy changed after original was recycled: %q", got)
	}
	q.releaseResendCopy(e.data)
}
