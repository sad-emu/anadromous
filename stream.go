//go:build linux

package anadromous

import (
	"crypto/subtle"
	"io"
	"sync"
	"time"
)

// xorInto XORs src into dst (dst[i] ^= src[i] for i < len(src)); dst must
// be at least as long as src. This runs once per DATA frame on the send path
// and once per accepted frame on the FEC receive path, so it has to move at
// memory speed: subtle.XORBytes is SIMD-accelerated (AVX2 on amd64), roughly
// 3× faster than a hand-rolled word-at-a-time loop — the difference is
// measurable in whole-connection throughput.
func xorInto(dst, src []byte) {
	subtle.XORBytes(dst[:len(src)], dst[:len(src)], src)
}

// reorderGraceFraction bounds how long a gap must persist — evidenced by a
// second out-of-order arrival for the same still-missing sequence — before
// it's treated as loss worth a fast retransmit. A single out-of-order
// arrival is ordinary evidence of packet reordering, not proof of loss
// (netem-style "packet jumps the queue" reordering typically resolves
// within one jitter window), so NACKing on first sight of a gap mostly
// fires on reordering rather than real loss and wastes bandwidth on
// duplicate resends over an already lossy link. The same duration throttles
// repeat NACKs for a gap that stays open. RTO/reorderGraceFraction is only
// the ceiling (and the pre-sample default): once RTT samples exist the
// grace adapts to the measured jitter instead — see Connection.reorderGrace.
const reorderGraceFraction = 4

// initialStreamWindow is the flow-control credit (and receive ring size) a
// stream starts with, before the ramp described below grows it.
const initialStreamWindow = 256 * 1024

// rampWindow is how long after a stream opens its window keeps doubling on a
// fixed pace (see rampIntervalFraction) instead of only when the ring is at
// least half full. Starting straight at cfg.streamBufSize (the operator's
// declared bandwidth-delay product) sounds appealing but is unsafe: nothing
// else in this protocol paces the sender, so an instantly-huge window lets
// it blast far more in-flight data than any real intermediate queue (a
// router buffer, or netem's default 1000-packet qdisc limit in testing) can
// hold, causing a burst of loss much worse than the link's steady-state
// rate and, if retransmits exhaust their retry budget during that burst,
// killing the stream outright. A bounded, paced ramp — doubling roughly
// every rampIntervalFraction of an RTO, the same way TCP slow start grows
// roughly every RTT — reaches a large window quickly without that burst.
const rampWindow = 3 * time.Second

// rampIntervalFraction sets the pace of the ramp above: doublings are at
// most retransmitTmout/rampIntervalFraction apart.
const rampIntervalFraction = 4

// Stream is a bidirectional byte stream multiplexed over a Connection.
//
// Semantics follow quic-go: Close only FINs the write direction (the read
// direction stays open until the peer FINs or resets), CancelWrite/CancelRead
// abort a direction, and a stream is garbage-collected once both directions
// have finished and all outgoing frames are acknowledged.
type Stream struct {
	id   uint32
	conn *Connection

	// read side — everything below is guarded by readMu.
	readMu         sync.Mutex
	readCond       *sync.Cond
	readBuf        []byte            // ring buffer; grows geometrically up to cfg.streamBufSize
	readHead       int               // next read position
	readTail       int               // next write position
	readLen        int               // buffered bytes count
	readSeq        uint32            // next expected sequence number
	reorder        map[uint32][]byte // out-of-order frames awaiting earlier seqs
	reorderBytes   int
	reorderAckSeqs []uint32                    // insertion-order ACK index; avoids map iteration in ackSnapshot
	reorderAckPos  int                         // rotating snapshot cursor into reorderAckSeqs
	reorderAckDead int                         // drained entries retained in reorderAckSeqs until compaction
	finRecvd       bool                        // peer sent FIN
	finSeq         uint32                      // seq position of the peer's FIN (== count of data frames)
	readErr        error                       // sticky read result: io.EOF, ErrStreamReset, ErrStreamClosed
	readCanceled   bool                        // CancelRead called: incoming data is ACKed and discarded
	reorderPool    [][]byte                    // free list reusing reorder-copy buffers, capped at maxPayload each
	grantPending   int                         // bytes consumed by the app since the last window grant
	peerOffset     int64                       // absolute watermark we've most recently told the peer it may send up to
	gapSeq         uint32                      // readSeq value of the gap currently being tracked for fast retransmit
	gapFirstSeenAt time.Time                   // when that gap was first observed
	lastNackAt     time.Time                   // when a NACK was last sent for the current gap
	lastArrivalAt  time.Time                   // last arrival while receive state was gapped; drives tail NACKs
	maxSeenSeq     uint32                      // highest data seq observed; bounds the NACK gap scan
	nackScratch    []uint32                    // reused gap-list buffer for NACK frames
	createdAt      time.Time                   // stream creation time; bounds the ramp-up window
	lastGrowAt     time.Time                   // when the ring last grew, paces the ramp-up
	fecAcc         map[uint32]*fecAccum        // FEC receiver: per-column-group running XOR of delivered members, keyed by group base
	fecPending     map[uint32]fecPendingParity // FEC receiver: column parity frames awaiting more group members, keyed by group base
	fecAccRow      map[uint32]*fecAccum        // FEC receiver: row-group counterpart of fecAcc (WithFEC2D)
	fecPendingRow  map[uint32]fecPendingParity // FEC receiver: row-group counterpart of fecPending (WithFEC2D)
	readDeadline   time.Time

	// write side — guarded by writeMu.
	writeMu       sync.Mutex
	writeCond     *sync.Cond
	writeSeq      uint32 // next data sequence number to send
	writeFIN      bool   // FIN (or reset) sent; no further writes
	writeErr      error  // sticky write error (ErrStreamReset, ErrStreamClosed, ...)
	maxSendOffset int64  // absolute watermark the peer has granted us; available credit is maxSendOffset-sentOffset
	sentOffset    int64  // cumulative bytes sent so far
	ackedBytes    int64  // cumulative bytes acknowledged so far (not necessarily contiguous — ACKs are selective)
	writeDeadline time.Time

	// FEC sender accumulators (writeMu): one lane per residue class of
	// seq mod G, so each parity group's members are strided G apart — see
	// Stream.fecFoldLocked. Under WithFEC2D, fecRowLane additionally folds
	// every chunk in sequence, emitting a parity over each G consecutive
	// seqs (the orthogonal "row" dimension).
	fecLanes   []fecLane
	fecRowLane fecLane
}

// fecLane accumulates one interleaved parity group: the running XOR of its
// members' data and lengths, the first member's seq (the group's wire
// identity), and how many members have been folded so far.
type fecLane struct {
	xor    []byte
	maxLen int
	lenXor uint32
	count  int
	base   uint32
}

func newStream(id uint32, conn *Connection, bufSize int) *Stream {
	initial := initialStreamWindow
	if bufSize < initial {
		initial = bufSize
	}
	// The ring must be able to hold at least a couple of full frames or
	// nothing can ever be delivered.
	if min := 2 * conn.cfg.maxPayload; initial < min {
		initial = min
	}
	now := time.Now()
	s := &Stream{
		id:            id,
		conn:          conn,
		readBuf:       make([]byte, initial),
		reorder:       make(map[uint32][]byte),
		peerOffset:    int64(initial),
		maxSendOffset: int64(initial),
		createdAt:     now,
		lastGrowAt:    now,
	}
	s.readCond = sync.NewCond(&s.readMu)
	s.writeCond = sync.NewCond(&s.writeMu)
	return s
}

// StreamID returns the stream's identifier.
func (s *Stream) StreamID() uint32 { return s.id }

// maxBufSize is the ceiling the receive ring may grow to.
func (s *Stream) maxBufSize() int {
	max := s.conn.cfg.streamBufSize
	if min := 2 * s.conn.cfg.maxPayload; max < min {
		max = min
	}
	return max
}

// --- deadline plumbing ---

// deadlineTimer wakes a cond-wait loop when a deadline passes. Re-arm on
// every loop iteration so deadline changes made while blocked take effect.
type deadlineTimer struct {
	timer    *time.Timer
	armedFor time.Time
}

func (d *deadlineTimer) arm(deadline time.Time, wake func()) {
	if deadline.Equal(d.armedFor) {
		return
	}
	d.stop()
	if !deadline.IsZero() {
		d.timer = time.AfterFunc(time.Until(deadline), wake)
		d.armedFor = deadline
	} else {
		d.armedFor = time.Time{}
	}
}

func (d *deadlineTimer) stop() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// --- Read ---

// Read reads up to len(p) bytes from the stream. Buffered data is always
// drained before a sticky error (io.EOF on clean FIN) is returned.
func (s *Stream) Read(p []byte) (n int, err error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	var dt deadlineTimer
	defer dt.stop()

	for s.readLen == 0 {
		if s.readErr != nil {
			return 0, s.readErr
		}
		if !s.readDeadline.IsZero() && !time.Now().Before(s.readDeadline) {
			return 0, errDeadlineExceeded
		}
		dt.arm(s.readDeadline, func() {
			s.readMu.Lock()
			s.readCond.Broadcast()
			s.readMu.Unlock()
		})
		s.readCond.Wait()
	}

	n = s.ringRead(p)
	s.drainReorderLocked()
	s.maybeEOFLocked()

	// Grant consumed bytes back to the peer once enough accumulate.
	s.grantPending += n
	if s.grantPending >= len(s.readBuf)/4 {
		credit := s.grantPending
		s.grantPending = 0
		s.grantLocked(credit)
	}
	return n, nil
}

// --- Write ---

// Write writes p to the stream, splitting it across DATA frames. It blocks
// when flow-control credit is exhausted and flushes the send batch once at
// the end, so a large write costs a single sendmmsg.
func (s *Stream) Write(p []byte) (n int, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var dt deadlineTimer
	defer dt.stop()

	maxChunk := s.conn.cfg.dataCap()
	for len(p) > 0 {
		chunkLen := len(p)
		if chunkLen > maxChunk {
			chunkLen = maxChunk
		}
		// Never overshoot the flow-control window (the receiver's ring is
		// sized exactly to the credit it has granted, so an oversized frame
		// would be dropped and limp in via retransmission), and never exceed
		// the in-flight cap: bound sent-but-unACKed bytes so a loss burst
		// makes Write block (backpressure) instead of piling arbitrarily
		// more outstanding retries on top of an already-struggling path.
		// The cap (see WithMaxBytesInFlight) is what keeps a drop burst
		// self-limiting rather than snowballing into persistent collapse.
		for s.writeErr == nil && !s.writeFIN &&
			(s.maxSendOffset-s.sentOffset < int64(chunkLen) ||
				s.bytesInFlightLocked()+int64(chunkLen) > s.inFlightCap()) {
			// Push anything queued before blocking — the credit we are
			// waiting for only arrives once the peer receives this data.
			s.conn.flushSend()
			if !s.writeDeadline.IsZero() && !time.Now().Before(s.writeDeadline) {
				return n, errDeadlineExceeded
			}
			dt.arm(s.writeDeadline, func() {
				s.writeMu.Lock()
				s.writeCond.Broadcast()
				s.writeMu.Unlock()
			})
			s.writeCond.Wait()
		}
		if s.writeErr != nil {
			s.conn.flushSend()
			return n, s.writeErr
		}
		if s.writeFIN {
			s.conn.flushSend()
			return n, ErrStreamFIN
		}

		chunk := p[:chunkLen]
		s.sentOffset += int64(chunkLen)
		seq := s.writeSeq
		s.writeSeq++

		if err = s.conn.sendDataFrame(s.id, seq, chunk); err != nil {
			s.conn.flushSend()
			return n, err
		}
		s.fecFoldLocked(seq, chunk)
		n += len(chunk)
		p = p[len(chunk):]
	}
	if ferr := s.conn.flushSend(); ferr != nil {
		return n, ferr
	}
	return n, nil
}

// fecFoldLocked folds a just-sent DATA frame into its parity lane's running
// XOR, emitting the lane's parity frame when its group fills. Lanes are the
// residue classes of seq mod G, so one group's members are strided G apart
// on the wire: a burst loss of up to G consecutive frames (e.g. one dropped
// GSO super-packet) puts at most ONE loss in each group, keeping every
// affected group single-loss-recoverable — consecutive grouping would
// instead concentrate the whole burst in one unrecoverable group.
//
// Under WithFEC2D every chunk is additionally folded into the rolling row
// lane, whose groups are the G consecutive seqs [kG, (k+1)G). Rows are the
// orthogonal complement of the strided columns: two losses in one column
// group land in two different rows (each often recoverable on its own), and
// each recovery can unblock the other dimension in turn — see the peeling
// retry in Stream.fecRetryBaseLocked. Because writes fold strictly
// sequentially, row bases stay aligned to multiples of G from seq 0, which
// is what lets the receiver key row groups statically. Caller holds writeMu.
func (s *Stream) fecFoldLocked(seq uint32, chunk []byte) {
	g := s.conn.cfg.fecGroup
	if g <= 0 {
		return
	}
	if s.fecLanes == nil {
		s.fecLanes = make([]fecLane, g)
	}
	s.foldLaneLocked(&s.fecLanes[seq%uint32(g)], seq, chunk, frameFEC)
	if s.conn.cfg.fec2D {
		s.foldLaneLocked(&s.fecRowLane, seq, chunk, frameFECRow)
	}
}

// foldLaneLocked folds one chunk into a lane, emitting when the group
// fills. Caller holds writeMu.
func (s *Stream) foldLaneLocked(ln *fecLane, seq uint32, chunk []byte, ftype uint8) {
	if ln.xor == nil {
		ln.xor = make([]byte, s.conn.cfg.dataCap())
	}
	if ln.count == 0 {
		ln.base = seq
	}
	xorInto(ln.xor, chunk)
	if len(chunk) > ln.maxLen {
		ln.maxLen = len(chunk)
	}
	ln.lenXor ^= uint32(len(chunk))
	ln.count++
	if ln.count >= s.conn.cfg.fecGroup {
		s.emitLaneLocked(ln, ftype)
	}
}

// emitLaneLocked sends a lane's (possibly partial) parity frame and resets
// the lane. Caller holds writeMu.
func (s *Stream) emitLaneLocked(ln *fecLane, ftype uint8) {
	if ln.count == 0 {
		return
	}
	s.conn.sendFecFrame(ftype, s.id, ln.base, ln.count, int(ln.lenXor), ln.xor[:ln.maxLen])
	clear(ln.xor[:ln.maxLen]) // compiles to memclr — an indexed byte loop here costs ~7% of whole-connection CPU
	ln.maxLen, ln.lenXor, ln.count = 0, 0, 0
}

// setMaxSendOffset applies an absolute flow-control watermark received from
// the peer. Only monotonic increases are applied: a reordered, duplicate, or
// stale (superseded) update is a harmless no-op rather than corrupting the
// window, which is what lets these updates go unretransmitted on a lossy
// link — see windowUpdatePayload.
func (s *Stream) setMaxSendOffset(offset int64) {
	s.writeMu.Lock()
	if offset > s.maxSendOffset {
		s.maxSendOffset = offset
		s.writeCond.Broadcast()
	}
	s.writeMu.Unlock()
}

// bytesInFlightLocked returns how many sent-but-not-yet-acknowledged bytes
// are currently outstanding for this stream. Caller holds writeMu.
func (s *Stream) bytesInFlightLocked() int64 {
	return s.sentOffset - s.ackedBytes
}

// inFlightCap is the bound Write enforces on bytesInFlight: the configured
// or RTT-selected derived cap (see Connection.inFlightCap), never more
// than the stream buffer size.
func (s *Stream) inFlightCap() int64 {
	cap := s.conn.inFlightCap()
	if max := int64(s.maxBufSize()); cap > max {
		cap = max
	}
	return cap
}

// creditAcked records newly-acknowledged bytes, shrinking bytesInFlight and
// waking any writer blocked on the in-flight cap in Write.
func (s *Stream) creditAcked(n int) {
	if n <= 0 {
		return
	}
	s.writeMu.Lock()
	s.ackedBytes += int64(n)
	s.writeCond.Broadcast()
	s.writeMu.Unlock()
}

// --- close / cancel ---

// Close closes the write direction of the stream (sends a FIN). The read
// direction is unaffected; it ends when the peer FINs or resets. This
// matches quic-go's Stream.Close semantics.
func (s *Stream) Close() error { return s.CloseWrite() }

// CloseWrite sends a FIN — signals that this side is done writing.
// The FIN occupies the sequence position after the last data frame and is
// retransmitted until acknowledged.
func (s *Stream) CloseWrite() error {
	s.writeMu.Lock()
	if s.writeFIN || s.writeErr != nil {
		s.writeMu.Unlock()
		return nil
	}
	s.writeFIN = true
	seq := s.writeSeq
	// Flush every lane's in-progress parity group (short groups are legal
	// on the wire) so the stream's final frames get FEC protection too; the
	// FIN itself is outside FEC (it's reliably retransmitted anyway).
	for i := range s.fecLanes {
		s.emitLaneLocked(&s.fecLanes[i], frameFEC)
	}
	s.emitLaneLocked(&s.fecRowLane, frameFECRow)
	s.writeCond.Broadcast()
	s.writeMu.Unlock()

	return s.conn.sendReliableFrame(frameStreamFIN, s.id, seq, nil)
}

// CancelWrite aborts the write direction: pending retransmits are dropped
// and the peer's read side is reset with the given application error code.
func (s *Stream) CancelWrite(code uint64) {
	s.abortWriteLocked(code, true)
}

// abortWriteLocked cancels the write side. If sendReset is true a RESET
// frame is sent to the peer (retransmitted until acknowledged).
func (s *Stream) abortWriteLocked(code uint64, sendReset bool) {
	s.writeMu.Lock()
	if s.writeFIN || s.writeErr != nil {
		// Already FIN'd or aborted — nothing to cancel.
		s.writeMu.Unlock()
		return
	}
	s.writeErr = ErrStreamReset
	s.writeFIN = true
	seq := s.writeSeq
	// Abandon the in-progress parity groups — the peer no longer wants them.
	s.fecLanes = nil
	s.fecRowLane = fecLane{}
	s.writeCond.Broadcast()
	s.writeMu.Unlock()

	// Drop any queued retransmits — the peer no longer wants this data —
	// then enqueue the reliable RESET at the next sequence position.
	s.conn.retransmit.purgeStream(s.id)
	if sendReset {
		s.conn.sendReliableFrame(frameStreamReset, s.id, seq, resetPayload(code))
	}
}

// CancelRead aborts the read direction: buffered and future incoming data is
// discarded (but still acknowledged, so the peer's retransmits stop), and the
// peer is asked to stop sending.
func (s *Stream) CancelRead(code uint64) {
	s.readMu.Lock()
	if s.readCanceled {
		s.readMu.Unlock()
		return
	}
	s.readCanceled = true
	if s.readErr == nil {
		s.readErr = ErrStreamReset
	}
	s.discardReadStateLocked()
	s.readCond.Broadcast()
	s.readMu.Unlock()

	s.conn.sendControlFrame(frameStopSending, s.id, uint32(code))
}

// discardReadStateLocked drops buffered ring, reorder, and FEC data. Caller
// holds readMu.
func (s *Stream) discardReadStateLocked() {
	s.readHead, s.readTail, s.readLen = 0, 0, 0
	s.reorder = make(map[uint32][]byte)
	s.reorderBytes = 0
	s.reorderAckSeqs = s.reorderAckSeqs[:0]
	s.reorderAckPos = 0
	s.reorderAckDead = 0
	s.fecAcc = nil
	s.fecPending = nil
	s.fecAccRow = nil
	s.fecPendingRow = nil
}

// SetDeadline sets both read and write deadlines.
func (s *Stream) SetDeadline(t time.Time) error {
	s.SetReadDeadline(t)
	s.SetWriteDeadline(t)
	return nil
}

// SetReadDeadline sets the read deadline. A zero value disables.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readMu.Lock()
	s.readDeadline = t
	s.readCond.Broadcast()
	s.readMu.Unlock()
	return nil
}

// SetWriteDeadline sets the write deadline. A zero value disables.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeMu.Lock()
	s.writeDeadline = t
	s.writeCond.Broadcast()
	s.writeMu.Unlock()
	return nil
}

// --- internal methods called by the Connection read loop ---

// deliver processes an incoming DATA or FIN frame for this stream and
// reports whether the frame should be acknowledged. Payload contents are
// copied before deliver returns (the underlying receive buffer is reused).
func (s *Stream) deliver(seq uint32, payload []byte, isFin bool) (ack bool) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	return s.deliverLocked(seq, payload, isFin)
}

// deliverLocked is deliver's body; FEC reconstruction (applyFec) calls it
// directly to inject a rebuilt frame while already holding readMu.
func (s *Stream) deliverLocked(seq uint32, payload []byte, isFin bool) (ack bool) {
	// While the receive state is gapped (or a FIN is pending), track
	// arrival times so the tail-NACK scan can detect silence — see
	// maybeTailNack. Clean in-order flow skips the clock read.
	if len(s.reorder) > 0 || s.finRecvd {
		s.lastArrivalAt = time.Now()
	}

	if s.readCanceled || s.readErr != nil {
		// Read side is done — discard, but acknowledge so the peer's
		// retransmits stop, and return the discarded bytes as credit so a
		// peer that missed our STOP_SENDING doesn't stall on flow control.
		// Acknowledgement happens by advancing readSeq past the discarded
		// frame: the cumulative watermark in our ACK snapshot then covers
		// it (and everything before it — frames we'll never deliver anyway).
		if len(payload) > 0 {
			s.grantLocked(len(payload))
		}
		if seq >= s.readSeq {
			s.readSeq = seq + 1
		}
		return true
	}

	if isFin {
		if seq < s.readSeq {
			return true // impossible FIN position; treat as duplicate
		}
		s.finRecvd = true
		s.finSeq = seq
		s.maybeEOFLocked()
		return true
	}

	if seq > s.maxSeenSeq {
		s.maxSeenSeq = seq
	}

	if seq < s.readSeq {
		return true // duplicate of already-delivered frame
	}
	if _, dup := s.reorder[seq]; dup {
		return true // duplicate of buffered out-of-order frame
	}

	if seq > s.readSeq {
		// Out of order: buffer a copy until the gap fills.
		if s.reorderBytes+len(payload) > s.maxBufSize() {
			return false // over budget; sender will retransmit
		}
		cp := s.reorderGetLocked(len(payload))
		copy(cp, payload)
		s.reorder[seq] = cp
		s.reorderAckSeqs = append(s.reorderAckSeqs, seq)
		s.reorderBytes += len(payload)
		s.conn.statReorder.Add(1)
		s.maybeFastRetransmitLocked()
		if s.conn.cfg.fecGroup > 0 {
			// A buffered member is visible to reconstruction (the reorder
			// map is consulted directly), so it may complete a pending group.
			s.fecRetrySeqLocked(seq)
		}
		return true
	}

	// In-order frame.
	if !s.ringFitsLocked(len(payload)) && !s.growLocked(len(payload)) {
		return false // no room; sender will retransmit
	}
	if s.conn.cfg.fecGroup > 0 {
		s.fecFoldRecvLocked(seq, payload)
	}
	s.ringWrite(payload)
	s.readSeq++
	s.drainReorderLocked()
	s.maybeGrowLocked()
	s.maybeEOFLocked()
	s.readCond.Broadcast()
	if s.conn.cfg.fecGroup > 0 {
		s.fecRetrySeqLocked(seq)
	}
	return true
}

// fecRetrySeqLocked re-attempts the pending parities of every group seq
// belongs to — its column group, and its row group under WithFEC2D. Caller
// holds readMu.
func (s *Stream) fecRetrySeqLocked(seq uint32) {
	base, _ := s.fecGroupOf(seq)
	s.fecRetryBaseLocked(false, base)
	if s.conn.cfg.fec2D {
		rowBase, _ := s.fecRowOf(seq)
		s.fecRetryBaseLocked(true, rowBase)
	}
}

// deliverReset handles an incoming RESET frame: the peer aborted its write
// side. Abortive per QUIC: buffered data is discarded.
func (s *Stream) deliverReset(code uint64) {
	_ = code
	s.readMu.Lock()
	if s.readErr == nil {
		s.readErr = ErrStreamReset
	}
	s.discardReadStateLocked()
	s.readCond.Broadcast()
	s.readMu.Unlock()
}

// onStopSending handles an incoming STOP_SENDING frame: the peer no longer
// wants our data, so cancel our write side and reset back (QUIC convention).
func (s *Stream) onStopSending(code uint64) {
	s.abortWriteLocked(code, true)
}

// deliverFIN is kept for the legacy direct call path (connection teardown of
// half-open peers); prefer deliver(seq, nil, true).
func (s *Stream) deliverFIN() {
	s.readMu.Lock()
	if s.readErr == nil {
		s.readErr = io.EOF
	}
	s.readCond.Broadcast()
	s.readMu.Unlock()
}

// deliverClose is called when the connection is torn down. Buffered data
// remains readable; everything else errors.
func (s *Stream) deliverClose() {
	s.readMu.Lock()
	if s.readErr == nil {
		s.readErr = ErrStreamClosed
	}
	s.readCond.Broadcast()
	s.readMu.Unlock()

	s.writeMu.Lock()
	if s.writeErr == nil {
		s.writeErr = ErrStreamClosed
	}
	s.writeFIN = true
	s.writeCond.Broadcast()
	s.writeMu.Unlock()
}

// deliverError fails the stream with a sticky error on both directions,
// e.g. when retransmission has been exhausted.
func (s *Stream) deliverError(err error) {
	s.readMu.Lock()
	if s.readErr == nil {
		s.readErr = err
	}
	s.readCond.Broadcast()
	s.readMu.Unlock()

	s.writeMu.Lock()
	if s.writeErr == nil {
		s.writeErr = err
	}
	s.writeFIN = true
	s.writeCond.Broadcast()
	s.writeMu.Unlock()
}

// finished reports whether both directions are complete: the write side has
// FIN'd or been reset, and the read side has reached a terminal state.
// Wire-level completion (all frames acknowledged) is checked separately by
// the connection's GC sweep via retransmitQueue.hasStream.
func (s *Stream) finished() bool {
	s.readMu.Lock()
	readDone := s.readErr != nil
	s.readMu.Unlock()
	if !readDone {
		return false
	}
	s.writeMu.Lock()
	writeDone := s.writeFIN || s.writeErr != nil
	s.writeMu.Unlock()
	return writeDone
}

// maybeEOFLocked delivers io.EOF once every data frame up to the peer's FIN
// position has been received. Caller holds readMu.
func (s *Stream) maybeEOFLocked() {
	if s.finRecvd && s.readSeq >= s.finSeq && s.readErr == nil {
		s.readErr = io.EOF
		s.readCond.Broadcast()
	}
}

// drainReorderLocked moves consecutive buffered frames into the ring.
// Caller holds readMu.
func (s *Stream) drainReorderLocked() {
	for {
		data, ok := s.reorder[s.readSeq]
		if !ok {
			return
		}
		if !s.ringFitsLocked(len(data)) && !s.growLocked(len(data)) {
			return // retry after the app reads (Read calls this too)
		}
		s.ringWrite(data)
		delete(s.reorder, s.readSeq)
		s.reorderAckDead++
		s.reorderBytes -= len(data)
		if s.conn.cfg.fecGroup > 0 {
			// The frame moves from the reorder map (where reconstruction
			// reads it directly) into its group's running XOR.
			s.fecFoldRecvLocked(s.readSeq, data)
		}
		s.reorderPutLocked(data)
		if len(s.reorder) == 0 {
			s.reorderAckSeqs = s.reorderAckSeqs[:0]
			s.reorderAckPos = 0
			s.reorderAckDead = 0
		}
		s.readSeq++
		s.readCond.Broadcast()
	}
}

// --- FEC receive side ---

// fecAccum is the receive-side complement of the sender's fecLane: the
// running XOR of a parity group's DELIVERED members. Members still sitting
// in the reorder map are not folded (reconstruction reads them from the map
// directly); a member is folded exactly once, at the moment it moves into
// the ring. Compared to caching every delivered frame's payload, this keeps
// one frame-sized buffer per active group (at most ~one per lane, since a
// group only needs an accumulator while it straddles readSeq) instead of
// O(G²) cached payloads, and replaces the per-frame cache copy with a
// SIMD XOR pass of the same cost.
type fecAccum struct {
	xor    []byte // pooled buffer, len == dataCap; running XOR of delivered members
	folded uint32 // bitmask of member indexes folded so far (fecGroup <= 32)
	lenXor uint32 // XOR of folded members' lengths
	maxLen int    // longest folded member
}

// fecGroupOf maps a DATA seq to its column parity group's base seq and its
// member index within the group. The sender's lanes make this static: lane
// l = seq%G receives every G-th seq and emits every G members without ever
// resetting mid-stream, so the group covering seq is
// base = (seq/G²)·G² + seq%G with member index (seq/G) mod G.
func (s *Stream) fecGroupOf(seq uint32) (base uint32, idx int) {
	g := uint32(s.conn.cfg.fecGroup)
	span := g * g
	return seq/span*span + seq%g, int(seq / g % g)
}

// fecRowOf is fecGroupOf's row-dimension counterpart: row groups are the G
// consecutive seqs [kG, (k+1)G), statically keyed because the sender folds
// strictly sequentially from seq 0.
func (s *Stream) fecRowOf(seq uint32) (base uint32, idx int) {
	g := uint32(s.conn.cfg.fecGroup)
	return seq / g * g, int(seq % g)
}

// fecFoldRecvLocked folds a frame being delivered into the ring into its
// column group's running XOR — and its row group's too under WithFEC2D.
// Caller holds readMu; only call when fecGroup > 0.
func (s *Stream) fecFoldRecvLocked(seq uint32, data []byte) {
	if len(data) > s.conn.cfg.dataCap() {
		return // oversized (non-conforming peer); leave its groups unprotected
	}
	base, idx := s.fecGroupOf(seq)
	s.fecAcc = s.fecFoldAccLocked(s.fecAcc, base, idx, data)
	if s.conn.cfg.fec2D {
		rowBase, rowIdx := s.fecRowOf(seq)
		s.fecAccRow = s.fecFoldAccLocked(s.fecAccRow, rowBase, rowIdx, data)
	}
	s.fecSweepLocked()
}

// fecFoldAccLocked folds one member into the group accumulator keyed base
// in accs, creating both as needed, and returns the (possibly newly
// allocated) map. Caller holds readMu.
func (s *Stream) fecFoldAccLocked(accs map[uint32]*fecAccum, base uint32, idx int, data []byte) map[uint32]*fecAccum {
	acc := accs[base]
	if acc == nil {
		if accs == nil {
			accs = make(map[uint32]*fecAccum, 2*s.conn.cfg.fecGroup)
		}
		buf := s.reorderGetLocked(s.conn.cfg.dataCap())
		n := copy(buf, data) // pooled buffer holds stale bytes: seed with the first member...
		clear(buf[n:])       // ...and zero the tail past it
		accs[base] = &fecAccum{xor: buf, folded: 1 << idx, lenXor: uint32(len(data)), maxLen: len(data)}
		return accs
	}
	if acc.folded&(1<<idx) != 0 || len(data) > len(acc.xor) {
		return accs // duplicate fold (can't happen — delivery is once per seq) or oversized
	}
	acc.folded |= 1 << idx
	xorInto(acc.xor, data)
	acc.lenXor ^= uint32(len(data))
	if len(data) > acc.maxLen {
		acc.maxLen = len(data)
	}
	return accs
}

// fecSweepLocked drops accumulators for groups whose every possible member
// is already delivered — a parity arriving for them is answered by the
// "readSeq past the last member" fast path in tryReconstructLocked without
// consulting an accumulator. Cheap when the maps are small, so it runs per
// fold; the maps stay at ~one live group per lane (columns) plus a couple
// of rows.
func (s *Stream) fecSweepLocked() {
	g := uint32(s.conn.cfg.fecGroup)
	if len(s.fecAcc) > 2*int(g) {
		span := g * g
		for base, acc := range s.fecAcc {
			if base+span-g < s.readSeq { // last possible member is base+G²-G
				s.reorderPutLocked(acc.xor)
				delete(s.fecAcc, base)
			}
		}
	}
	if len(s.fecAccRow) > 2*int(g) {
		for base, acc := range s.fecAccRow {
			if base+g-1 < s.readSeq { // last possible member is base+G-1
				s.reorderPutLocked(acc.xor)
				delete(s.fecAccRow, base)
			}
		}
	}
}

// fecRetryBaseLocked re-attempts a buffered parity for one group after a
// member of that group became available (delivered into the accumulator, or
// inserted into the reorder map). This is also the peeling engine for 2D
// FEC: a frame recovered in one dimension is injected via deliverLocked,
// whose own pass through the fold/retry hooks re-attempts the OTHER
// dimension's pending parity, chaining recoveries. Caller holds readMu.
func (s *Stream) fecRetryBaseLocked(row bool, base uint32) {
	pending := s.fecPending
	if row {
		pending = s.fecPendingRow
	}
	p, ok := pending[base]
	if !ok {
		return
	}
	// Claim the entry before reconstructing so the injected frame's own
	// pass through this hook can't double-process it.
	delete(pending, base)
	switch s.tryReconstructLocked(row, base, p.count, p.xorLen, p.data) {
	case fecPendingMembers:
		pending[base] = p // still waiting; put it back
	case fecRecovered:
		s.conn.statFecRecovered.Add(1)
		s.reorderPutLocked(p.data)
	default:
		s.reorderPutLocked(p.data)
	}
}

// fecPendingParity is a buffered parity frame whose group couldn't be
// reconstructed yet (two or more members still in flight, typically because
// jitter delivered the parity ahead of its members). It's retried as each
// member arrives — see the retry hook in deliverLocked.
type fecPendingParity struct {
	count  int
	xorLen int
	data   []byte // pooled copy
}

// fecPendingCap bounds each dimension's pending-parity buffer. Sized for
// big-window lossy links: a 32MB window spans ~60 column groups, and at
// ~10% loss a fifth of them are legitimately pending at once — a cap near
// that count evicts parities that would still have recovered frames once
// their members arrived. Worst case cost per stream per dimension is
// fecPendingCap pooled frame-size buffers (~0.5MB at the defaults).
const fecPendingCap = 64

// Reconstruction outcomes for tryReconstructLocked.
const (
	fecUseless        = iota // group complete, unrecoverable, or parity invalid — parity can be dropped
	fecRecovered             // the missing member was rebuilt and injected
	fecPendingMembers        // >1 member still in flight — worth retrying later
)

// applyFec processes a parity frame covering count DATA seqs from base —
// strided G apart for column parity (row=false), consecutive for row parity
// (row=true, WithFEC2D): if exactly one member is missing and every other
// member's data is still available (in the reorder map or the group's
// running-XOR accumulator), the missing frame is reconstructed by XOR and
// injected as if it had arrived — zero-round-trip loss recovery. A parity
// whose group still has several members in flight is buffered and retried
// as members arrive. Returns whether a frame was recovered right now.
// Idempotent: on a duplicate parity, nothing is missing anymore.
func (s *Stream) applyFec(row bool, base uint32, count, xorLen int, xorData []byte) (recovered bool) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.readCanceled || s.readErr != nil {
		return false
	}
	g := uint32(s.conn.cfg.fecGroup)
	if row {
		if base%g != 0 {
			return false // row bases are multiples of G: malformed
		}
	} else if base%(g*g) >= g {
		return false // base doesn't sit on our lane geometry: malformed
	}
	switch s.tryReconstructLocked(row, base, count, xorLen, xorData) {
	case fecRecovered:
		return true
	case fecPendingMembers:
		pending := s.fecPending
		if row {
			if s.fecPendingRow == nil {
				s.fecPendingRow = make(map[uint32]fecPendingParity)
			}
			pending = s.fecPendingRow
		} else if pending == nil {
			s.fecPending = make(map[uint32]fecPendingParity)
			pending = s.fecPending
		}
		if old, ok := pending[base]; ok {
			s.reorderPutLocked(old.data)
		} else if len(pending) >= fecPendingCap {
			// Bound the buffer: evict the oldest group.
			oldest := ^uint32(0)
			for k := range pending {
				if k < oldest {
					oldest = k
				}
			}
			s.reorderPutLocked(pending[oldest].data)
			delete(pending, oldest)
		}
		cp := s.reorderGetLocked(len(xorData))
		copy(cp, xorData)
		pending[base] = fecPendingParity{count: count, xorLen: xorLen, data: cp}
	}
	return false
}

// tryReconstructLocked attempts the group reconstruction described on
// applyFec, for a column group (row=false, members strided G apart) or a
// row group (row=true, consecutive members). Delivered members are read
// from the group's accumulator (all at once — they're pre-XORed), buffered
// members from the reorder map. A member that is neither folded, nor
// buffered, nor still in flight (seq < readSeq) means the accumulator was
// swept — recovery is impossible but, crucially, never wrong: a stale or
// recreated accumulator can only make delivered members look missing,
// which lands in this bail-out or the two-missing pending path, never in a
// garbage reconstruction. Caller holds readMu.
func (s *Stream) tryReconstructLocked(row bool, base uint32, count, xorLen int, xorData []byte) int {
	stride := uint32(s.conn.cfg.fecGroup)
	accs := s.fecAcc
	if row {
		stride = 1
		accs = s.fecAccRow
	}
	if s.readSeq > base+uint32(count-1)*stride {
		return fecUseless // every member already delivered
	}
	acc := accs[base]
	var folded uint32
	if acc != nil {
		if acc.maxLen > len(xorData) {
			return fecUseless // a delivered member outsizes the parity: mismatched
		}
		folded = acc.folded
	}
	missing := -1
	for i := 0; i < count; i++ {
		if folded&(1<<i) != 0 {
			continue
		}
		seq := base + uint32(i)*stride
		if _, ok := s.reorder[seq]; ok {
			continue
		}
		if seq < s.readSeq {
			return fecUseless // delivered but its accumulator was swept — can't XOR
		}
		if missing >= 0 {
			return fecPendingMembers
		}
		missing = i
	}
	if missing < 0 {
		return fecUseless
	}

	buf := s.reorderGetLocked(len(xorData))
	copy(buf, xorData)
	length := uint32(xorLen)
	if acc != nil {
		xorInto(buf, acc.xor[:acc.maxLen])
		length ^= acc.lenXor
	}
	for i := 0; i < count; i++ {
		if folded&(1<<i) != 0 || i == missing {
			continue
		}
		data := s.reorder[base+uint32(i)*stride]
		if len(data) > len(buf) {
			s.reorderPutLocked(buf)
			return fecUseless // buffered member outsizes the parity: mismatched
		}
		xorInto(buf, data)
		length ^= uint32(len(data))
	}
	// A zero length would mean the missing member carried no data — only
	// FIN/RESET occupy seqs without data, and those are reliably
	// retransmitted, so don't guess; and an implausible length means the
	// parity didn't match this group's reality (e.g. stale config) —
	// discard rather than inject garbage.
	if length == 0 || int(length) > len(xorData) {
		s.reorderPutLocked(buf)
		return fecUseless
	}
	s.deliverLocked(base+uint32(missing)*stride, buf[:length], false)
	s.reorderPutLocked(buf) // deliverLocked copied what it kept
	return fecRecovered
}

// reorderGetLocked returns a length-n buffer for an out-of-order frame copy,
// reusing a pooled one when available. Under a loss burst the reorder path
// runs once per buffered frame, and per-frame allocations here feed the GC
// exactly when the receiver can least afford pauses — a stalled receiver
// drops more packets, which buffers more frames, which allocates more.
// Caller holds readMu.
func (s *Stream) reorderGetLocked(n int) []byte {
	if n <= s.conn.cfg.maxPayload {
		if l := len(s.reorderPool); l > 0 {
			buf := s.reorderPool[l-1]
			s.reorderPool[l-1] = nil
			s.reorderPool = s.reorderPool[:l-1]
			return buf[:n]
		}
		return make([]byte, n, s.conn.cfg.maxPayload)
	}
	return make([]byte, n) // over-size: not poolable, GC-owned
}

// reorderPutLocked returns a drained reorder buffer to the pool. Caller
// holds readMu.
func (s *Stream) reorderPutLocked(buf []byte) {
	if cap(buf) != s.conn.cfg.maxPayload {
		return // the over-size fallback from reorderGetLocked
	}
	s.reorderPool = append(s.reorderPool, buf[:0:s.conn.cfg.maxPayload])
}

// --- ring buffer (all-or-nothing, growable) ---

func (s *Stream) ringFitsLocked(n int) bool {
	return s.readLen+n <= len(s.readBuf)
}

// growLocked grows the ring so that need more bytes fit, doubling capacity
// up to maxBufSize. Grants the capacity delta to the peer as flow-control
// credit. Returns false if the ceiling has been reached.
func (s *Stream) growLocked(need int) bool {
	oldCap := len(s.readBuf)
	newCap := oldCap
	max := s.maxBufSize()
	for newCap < s.readLen+need {
		newCap *= 2
	}
	if newCap > max {
		newCap = max
	}
	if newCap <= oldCap || s.readLen+need > newCap {
		return false
	}
	s.reallocRingLocked(newCap)
	s.grantLocked(newCap - oldCap)
	return true
}

// maybeGrowLocked doubles the ring, granting the delta as credit, so
// throughput ramps up to the configured buffer size without waiting for a
// stall. Growth fires for either of two reasons: the ring is at least half
// full (the original occupancy signal — the current window is visibly not
// enough), or the stream is still within its early rampWindow and has gone
// at least one ramp interval since its last growth (a fixed, paced climb
// towards the configured ceiling that doesn't wait to be proven necessary,
// but also doesn't hand it all out at once — see rampWindow).
func (s *Stream) maybeGrowLocked() {
	oldCap := len(s.readBuf)
	if oldCap >= s.maxBufSize() {
		return
	}
	now := time.Now()
	occupied := s.readLen*2 >= oldCap
	ramping := now.Sub(s.createdAt) < rampWindow &&
		now.Sub(s.lastGrowAt) >= s.conn.currentRTO()/rampIntervalFraction
	if !occupied && !ramping {
		return
	}
	newCap := oldCap * 2
	if max := s.maxBufSize(); newCap > max {
		newCap = max
	}
	s.lastGrowAt = now
	s.reallocRingLocked(newCap)
	s.grantLocked(newCap - oldCap)
}

// maxNackSeqsPerFire bounds how many missing frames one loss-detection firing
// asks for. The volley is split across existing-format NACK frames as needed.
// A large window
// on a lossy link can hold hundreds of concurrent gaps; a small volley
// fast-retransmits only the head-most few and leaves the rest to the far
// slower RTO timer, so the budget should comfortably exceed the plausible
// concurrent-loss population. Over-asking is cheap since the sender
// suppresses resends of anything re-sent within the last ¾·SRTT (see
// retransmitQueue.requestForResend) — a mistaken or repeated volley no longer
// multiplies duplicate traffic.
const maxNackSeqsPerFire = 1024

// maybeFastRetransmitLocked asks the peer to immediately resend frames we
// have reasonable evidence are actually lost rather than merely reordered:
// the head gap (s.readSeq) has persisted for a grace period since first
// observed, re-confirmed by this latest out-of-order arrival. This recovers
// lost DATA frames in a small multiple of the RTT instead of waiting for
// the retransmit timeout — the same idea as TCP fast-retransmit / QUIC's
// ACK-driven loss detection — while the grace period keeps ordinary packet
// reordering from being mistaken for loss.
//
// The NACK carries EVERY missing seq up to maxSeenSeq (bounded by
// maxNackSeqsPerFire), not just the head gap: loss is frequently bursty —
// correlated drops, and with GSO an entire super-packet of consecutive
// frames vanishes as a unit — and asking for one frame per grace period
// would heal an N-frame hole serially in N round trips, stalling delivery
// far longer than the single round trip the whole volley needs. Caller
// holds readMu.
func (s *Stream) maybeFastRetransmitLocked() {
	now := time.Now()
	s.lastArrivalAt = now // an out-of-order arrival is still an arrival
	grace := s.conn.reorderGrace()

	if s.readSeq != s.gapSeq {
		// First sign of a new gap at this readSeq: start the clock, but
		// don't NACK yet.
		s.gapSeq = s.readSeq
		s.gapFirstSeenAt = now
		return
	}
	if now.Sub(s.gapFirstSeenAt) < grace || now.Sub(s.lastNackAt) < grace {
		return
	}
	s.lastNackAt = now
	s.fireNackLocked()
}

// maybeTailNack is the silence-driven counterpart of fast retransmit,
// called periodically from the connection's retransmit loop: arrival-driven
// NACKs only fire when a later frame ARRIVES to reveal a gap, so a lost
// burst tail (nothing sent after it) or a fully-lost batch would otherwise
// wait out the whole retransmit timeout. If the stream knows it has a gap —
// buffered out-of-order frames, or a FIN whose preceding data hasn't all
// arrived — and nothing has arrived for a grace period, NACK the missing
// frames without waiting for further evidence.
func (s *Stream) maybeTailNack(now time.Time) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.readErr != nil || s.readCanceled {
		return
	}
	gap := len(s.reorder) > 0 || (s.finRecvd && s.readSeq < s.finSeq)
	if !gap || s.lastArrivalAt.IsZero() {
		return
	}
	grace := s.conn.reorderGrace()
	if now.Sub(s.lastArrivalAt) < grace || now.Sub(s.lastNackAt) < grace {
		return
	}
	s.lastNackAt = now
	s.fireNackLocked()
}

// fireNackLocked sends a NACK listing every missing seq up to the highest
// evidence of sent data: the highest seq seen, or the FIN position when the
// peer has already FINed (covering tail frames that were lost without any
// later arrival to reveal them). Caller holds readMu and has applied the
// grace/throttle checks.
func (s *Stream) fireNackLocked() {
	gaps := s.nackScratch[:0]
	max := maxNackSeqsPerFire
	hi := s.maxSeenSeq
	if s.finRecvd && s.finSeq > 0 && s.finSeq-1 > hi {
		hi = s.finSeq - 1
	}
	for seq := s.readSeq; seq <= hi && len(gaps) < max; seq++ {
		if _, buffered := s.reorder[seq]; !buffered {
			gaps = append(gaps, seq)
		}
	}
	s.nackScratch = gaps
	if len(gaps) > 0 {
		s.conn.sendNackFrames(s.id, gaps)
	}
}

// ackSnapshot returns the stream's receive state for ACK frames: the
// cumulative contiguous watermark (every seq below it received or
// discarded), the seqs received beyond it — the buffered out-of-order
// frames, plus the FIN's seq once seen — and the current flow-control
// watermark granted to the peer (re-advertised alongside the ACKs so a
// lost WINDOW_UPDATE heals within a batch instead of stalling the sender
// until the next grant). Because it describes state rather than events,
// the resulting frames are idempotent: re-advertised on every flush, so
// any single lost frame is healed by the next one. seqs reuses scratch's
// backing array and is capped at maxSeqs. reorderAckSeqs is a rotating
// insertion-order index over the reorder map: it avoids map-iterator setup
// on every receive batch and ensures successive capped snapshots cover every
// buffered frame deterministically. Drained index entries are compacted
// amortized, so the index does not grow for the life of a stream.
func (s *Stream) ackSnapshot(scratch []uint32, maxSeqs int) (cum uint32, seqs []uint32, granted int64) {
	s.readMu.Lock()
	cum = s.readSeq
	granted = s.peerOffset
	seqs = scratch[:0]
	if s.finRecvd && s.finSeq >= cum && len(seqs) < maxSeqs {
		seqs = append(seqs, s.finSeq)
	}
	if s.reorderAckDead >= 64 && s.reorderAckDead*2 >= len(s.reorderAckSeqs) {
		live := s.reorderAckSeqs[:0]
		for _, seq := range s.reorderAckSeqs {
			if _, ok := s.reorder[seq]; ok {
				live = append(live, seq)
			}
		}
		s.reorderAckSeqs = live
		s.reorderAckPos = 0
		s.reorderAckDead = 0
	}
	count := len(s.reorderAckSeqs)
	if count > 0 && len(seqs) < maxSeqs {
		start := s.reorderAckPos % count
		scanned := 0
		for scanned < count && len(seqs) < maxSeqs {
			seq := s.reorderAckSeqs[(start+scanned)%count]
			if _, ok := s.reorder[seq]; ok {
				seqs = append(seqs, seq)
			}
			scanned++
		}
		s.reorderAckPos = (start + scanned) % count
	}
	s.readMu.Unlock()
	return
}

// grantLocked increases the cumulative offset granted to the peer by delta
// bytes and sends the new absolute watermark. Caller holds readMu.
func (s *Stream) grantLocked(delta int) {
	if delta <= 0 {
		return
	}
	s.peerOffset += int64(delta)
	s.conn.sendWindowUpdate(s.id, uint64(s.peerOffset))
}

// reallocRingLocked linearizes the ring contents into a new buffer of the
// given capacity. Caller holds readMu; newCap must be >= readLen.
func (s *Stream) reallocRingLocked(newCap int) {
	nb := make([]byte, newCap)
	n := s.ringRead(nb[:s.readLen])
	s.readBuf = nb
	s.readHead = 0
	s.readTail = n % newCap
	s.readLen = n
}

// ringWrite copies data into the ring. Caller must have checked ringFitsLocked.
func (s *Stream) ringWrite(data []byte) {
	cap_ := len(s.readBuf)
	n := len(data)
	end := s.readTail + n
	if end <= cap_ {
		copy(s.readBuf[s.readTail:end], data)
	} else {
		first := cap_ - s.readTail
		copy(s.readBuf[s.readTail:cap_], data[:first])
		copy(s.readBuf[:end-cap_], data[first:])
	}
	s.readTail = (s.readTail + n) % cap_
	s.readLen += n
}

func (s *Stream) ringRead(p []byte) int {
	cap_ := len(s.readBuf)
	n := len(p)
	if n > s.readLen {
		n = s.readLen
	}
	end := s.readHead + n
	if end <= cap_ {
		copy(p[:n], s.readBuf[s.readHead:end])
	} else {
		first := cap_ - s.readHead
		copy(p[:first], s.readBuf[s.readHead:cap_])
		copy(p[first:n], s.readBuf[:end-cap_])
	}
	s.readHead = (s.readHead + n) % cap_
	s.readLen -= n
	return n
}

var errDeadlineExceeded = &deadlineError{}

type deadlineError struct{}

func (e *deadlineError) Error() string   { return "anadromous: deadline exceeded" }
func (e *deadlineError) Timeout() bool   { return true }
func (e *deadlineError) Temporary() bool { return true }
