//go:build linux

package anadromous

import (
	"sync"
	"time"
)

// retransmit.go implements fixed-interval, no-backoff retransmission of
// unacknowledged DATA frames. There is deliberately no exponential backoff
// or congestion-driven pacing here: this protocol targets WAN links with
// known capacity, so the retry interval stays constant regardless of loss.

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
	retries  int
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

// ackMany removes a batch of acknowledged frames for one stream.
func (q *retransmitQueue) ackMany(streamID uint32, seqs []uint32) {
	q.mu.Lock()
	for _, seq := range seqs {
		delete(q.entries, retransmitKey(streamID, seq))
	}
	q.mu.Unlock()
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

// due returns a snapshot of entries that have been outstanding for at least
// rto, and are still under maxRetries. Their sentAt/retries are bumped in
// place so a subsequent scan won't immediately re-select them. Entries that
// have exceeded maxRetries are removed and returned separately via exceeded.
func (q *retransmitQueue) due(rto time.Duration, maxRetries int) (resend []retransmitEntry, exceeded []retransmitEntry) {
	now := time.Now()
	q.mu.Lock()
	for k, e := range q.entries {
		if now.Sub(e.sentAt) < rto {
			continue
		}
		if e.retries >= maxRetries {
			exceeded = append(exceeded, *e)
			delete(q.entries, k)
			continue
		}
		e.sentAt = now
		e.retries++
		resend = append(resend, *e)
	}
	q.mu.Unlock()
	return
}
