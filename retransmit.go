//go:build linux

package anadromous

import (
	"sync"
	"time"
)

// retransmit.go implements retransmission of unacknowledged DATA frames on
// an interval that adapts to the connection's measured RTT (SRTT/RTTVAR via
// RFC 6298 — see Connection.updateRTO), rather than a fixed guess. There is
// still no exponential per-attempt backoff or congestion-driven pacing here:
// repeated retries of the same frame all use the same connection-wide
// adaptive interval rather than growing it individually.
//
// A frame is retried for as long as the connection itself is alive — there
// is deliberately no separate per-frame retry-count give-up. An earlier
// version failed a stream with ErrRetransmitExceeded after a fixed number of
// attempts, but a single frame struggling under a transient, correlated loss
// burst (common on bad WAN links, and easily several seconds long) isn't
// evidence the peer is gone, and killing the stream over it tore down
// whatever was on top of it (e.g. a TCP connection being bridged) even
// though the network — and the transfer — would have recovered shortly
// after. Whether the peer is actually gone is exactly what the connection's
// idle timeout already determines, correctly and robustly (it resets on any
// received frame, not just a dedicated pong — see handleDatagram); that is
// now the only mechanism that gives up on a stalled connection, and it takes
// every stream down with it via Connection.Close.

// retransmitEntry tracks a single sent frame pending acknowledgement.
// Besides DATA frames this also covers FIN and RESET frames, which occupy
// the sequence position after the stream's last data frame and are ACKed
// through the same per-stream sequence space.
type retransmitEntry struct {
	ftype    uint8
	streamID uint32
	seq      uint32
	data     []byte // owned copy of the payload
	sentAt   time.Time
	retries  int // resend attempts so far, diagnostic only
}

// retransmitQueue manages pending unacknowledged frames for a Connection.
type retransmitQueue struct {
	mu      sync.Mutex
	entries map[uint64]*retransmitEntry
}

func newRetransmitQueue() *retransmitQueue {
	return &retransmitQueue{entries: make(map[uint64]*retransmitEntry)}
}

func retransmitKey(streamID, seq uint32) uint64 {
	return uint64(streamID)<<32 | uint64(seq)
}

// add records a sent frame as pending acknowledgement. payload is copied.
func (q *retransmitQueue) add(ftype uint8, streamID, seq uint32, payload []byte) {
	data := make([]byte, len(payload))
	copy(data, payload)

	q.mu.Lock()
	q.entries[retransmitKey(streamID, seq)] = &retransmitEntry{
		ftype:    ftype,
		streamID: streamID,
		seq:      seq,
		data:     data,
		sentAt:   time.Now(),
	}
	q.mu.Unlock()
}

// hasStream reports whether any entries remain pending for a stream.
func (q *retransmitQueue) hasStream(streamID uint32) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.entries {
		if e.streamID == streamID {
			return true
		}
	}
	return false
}

// ackOne removes a single acknowledged frame from the queue.
func (q *retransmitQueue) ackOne(streamID, seq uint32) {
	q.mu.Lock()
	delete(q.entries, retransmitKey(streamID, seq))
	q.mu.Unlock()
}

// ackMany removes a batch of acknowledged frames for one stream, returning
// the total payload size of the removed entries so the caller can shrink the
// stream's bytes-in-flight accounting (see Stream.creditAcked), plus an RTT
// sample for each entry that was ACKed without ever being retransmitted.
// Retransmitted entries are excluded per Karn's algorithm: an ACK for a
// multiply-sent frame can't be attributed to a specific transmission, so
// timing it would poison the RTO estimate.
func (q *retransmitQueue) ackMany(streamID uint32, seqs []uint32) (freedBytes int, rttSamples []time.Duration) {
	now := time.Now()
	q.mu.Lock()
	for _, seq := range seqs {
		key := retransmitKey(streamID, seq)
		if e, ok := q.entries[key]; ok {
			freedBytes += len(e.data)
			if e.retries == 0 {
				rttSamples = append(rttSamples, now.Sub(e.sentAt))
			}
			delete(q.entries, key)
		}
	}
	q.mu.Unlock()
	return
}

// purgeStream removes all pending entries belonging to a stream, e.g. when
// the stream is closed or torn down.
func (q *retransmitQueue) purgeStream(streamID uint32) {
	q.mu.Lock()
	for k, e := range q.entries {
		if e.streamID == streamID {
			delete(q.entries, k)
		}
	}
	q.mu.Unlock()
}

// getForResend returns a copy of the pending entry for (streamID, seq) for
// an immediate fast-retransmit resend, bumping its sentAt/retries exactly
// like due() would, so the periodic scanner doesn't immediately re-select it
// too. ok is false if the frame isn't outstanding (already acknowledged, or
// a stale/unknown NACK) — callers must treat that as a no-op, not an error.
func (q *retransmitQueue) getForResend(streamID, seq uint32) (e retransmitEntry, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := retransmitKey(streamID, seq)
	entry, found := q.entries[key]
	if !found {
		return retransmitEntry{}, false
	}
	entry.sentAt = time.Now()
	entry.retries++
	return *entry, true
}

// due returns a snapshot of entries that have been outstanding (since their
// last resend) for at least rto. Their sentAt/retries are bumped in place so
// a subsequent scan won't immediately re-select them.
func (q *retransmitQueue) due(rto time.Duration) (resend []retransmitEntry) {
	now := time.Now()
	q.mu.Lock()
	for _, e := range q.entries {
		if now.Sub(e.sentAt) < rto {
			continue
		}
		e.sentAt = now
		e.retries++
		resend = append(resend, *e)
	}
	q.mu.Unlock()
	return
}
