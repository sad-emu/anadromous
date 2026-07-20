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
	data     []byte // payload copy, owned by the arena — do not retain past removal
	sentAt   time.Time
	retries  int // resend attempts so far, diagnostic only
}

// retransmitArena is a self-managed free list of fixed-size buffers used to
// hold pending frames' payload copies, so the send hot path doesn't call the
// allocator (and the GC doesn't have to collect) once per DATA frame. Most
// frames are ACKed and removed within a few RTTs of being sent, so the
// working set of buffers needed at any moment is small and roughly
// constant; the arena grows to that size once (in batches, to keep the
// allocation count low) and then reuses the same buffers indefinitely.
// Callers serialize access via retransmitQueue.mu — the arena has no lock of
// its own.
type retransmitArena struct {
	slotSize int
	growBy   int
	free     [][]byte
}

func newRetransmitArena(slotSize int) *retransmitArena {
	return &retransmitArena{slotSize: slotSize, growBy: 128}
}

// grow adds growBy fresh slots to the free list from one shared backing
// allocation, rather than one allocation per slot.
func (a *retransmitArena) grow() {
	backing := make([]byte, a.slotSize*a.growBy)
	for i := 0; i < a.growBy; i++ {
		start := i * a.slotSize
		a.free = append(a.free, backing[start:start:start+a.slotSize])
	}
}

// get returns a buffer with length n, reused from the free list when
// available. n must not exceed slotSize — DATA frames are already capped at
// cfg.maxPayload (the arena's slot size), but a payload that somehow
// exceeds it falls back to a direct allocation rather than corrupting
// accounting; that buffer is simply not poolable (see put).
func (a *retransmitArena) get(n int) []byte {
	if n > a.slotSize {
		return make([]byte, n)
	}
	if len(a.free) == 0 {
		a.grow()
	}
	l := len(a.free)
	buf := a.free[l-1]
	a.free[l-1] = nil
	a.free = a.free[:l-1]
	return buf[:n]
}

// put returns a buffer obtained from get back to the free list for reuse.
func (a *retransmitArena) put(buf []byte) {
	if cap(buf) != a.slotSize {
		return // the oversized fallback from get — not one of ours, let the GC take it
	}
	a.free = append(a.free, buf[:0:a.slotSize])
}

// retransmitQueue manages pending unacknowledged frames for a Connection.
//
// pendingFree holds buffers whose entries have been ACKed or purged but
// aren't back in the arena's free list yet — see drainPendingFree.
type retransmitQueue struct {
	mu          sync.Mutex
	entries     map[uint64]*retransmitEntry
	arena       *retransmitArena
	pendingFree [][]byte
}

func newRetransmitQueue(maxPayload int) *retransmitQueue {
	return &retransmitQueue{
		entries: make(map[uint64]*retransmitEntry),
		arena:   newRetransmitArena(maxPayload),
	}
}

func retransmitKey(streamID, seq uint32) uint64 {
	return uint64(streamID)<<32 | uint64(seq)
}

// add records a sent frame as pending acknowledgement, copying payload into
// an arena-owned buffer, and returns that buffer so the caller can send it
// by reference (see Connection.commitSendSlotZeroCopy) — the payload is
// written here once and not copied again for as long as it's outstanding,
// including across retransmissions.
func (q *retransmitQueue) add(ftype uint8, streamID, seq uint32, payload []byte) []byte {
	q.mu.Lock()
	data := q.arena.get(len(payload))
	copy(data, payload)
	q.entries[retransmitKey(streamID, seq)] = &retransmitEntry{
		ftype:    ftype,
		streamID: streamID,
		seq:      seq,
		data:     data,
		sentAt:   time.Now(),
	}
	q.mu.Unlock()
	return data
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
	key := retransmitKey(streamID, seq)
	if e, ok := q.entries[key]; ok {
		q.pendingFree = append(q.pendingFree, e.data)
		delete(q.entries, key)
	}
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
			q.pendingFree = append(q.pendingFree, e.data)
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
			q.pendingFree = append(q.pendingFree, e.data)
			delete(q.entries, k)
		}
	}
	q.mu.Unlock()
}

// drainPendingFree moves buffers freed by ackOne/ackMany/purgeStream since
// the last drain into the arena's real free list, making them available to
// get. This must only be called once the caller can guarantee that any
// sendmmsg batch which might still reference those buffers by pointer (via
// commitSendSlotZeroCopy) has already been handed to the kernel — otherwise
// a buffer could be recycled into an unrelated frame while an unflushed send
// still points at it, corrupting that send. Connection.flushSendLocked is
// the only caller, immediately after its SendMMsgRetry call returns: at that
// point every zero-copy pointer in the batch just sent has already been
// consumed by the kernel, so anything freed before this drain is safe to
// reuse, and anything freed after will simply wait for the next drain.
func (q *retransmitQueue) drainPendingFree() {
	q.mu.Lock()
	for _, buf := range q.pendingFree {
		q.arena.put(buf)
	}
	q.pendingFree = q.pendingFree[:0]
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
