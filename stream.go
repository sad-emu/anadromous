//go:build linux

package anadromous

import (
	"io"
	"sync"
	"time"
)

// initialStreamWindow is the flow-control credit (and receive ring size) a
// stream starts with. The receive ring grows on demand up to the configured
// stream buffer size, granting the peer additional credit as it does.
const initialStreamWindow = 256 * 1024

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
	readMu       sync.Mutex
	readCond     *sync.Cond
	readBuf      []byte            // ring buffer; grows geometrically up to cfg.streamBufSize
	readHead     int               // next read position
	readTail     int               // next write position
	readLen      int               // buffered bytes count
	readSeq      uint32            // next expected sequence number
	reorder      map[uint32][]byte // out-of-order frames awaiting earlier seqs
	reorderBytes int
	finRecvd     bool   // peer sent FIN
	finSeq       uint32 // seq position of the peer's FIN (== count of data frames)
	readErr      error  // sticky read result: io.EOF, ErrStreamReset, ErrStreamClosed
	readCanceled bool   // CancelRead called: incoming data is ACKed and discarded
	grantPending int    // bytes consumed by the app since the last window grant
	peerOffset   int64  // absolute watermark we've most recently told the peer it may send up to
	readDeadline time.Time

	// write side — guarded by writeMu.
	writeMu       sync.Mutex
	writeCond     *sync.Cond
	writeSeq      uint32 // next data sequence number to send
	writeFIN      bool   // FIN (or reset) sent; no further writes
	writeErr      error  // sticky write error (ErrStreamReset, ErrStreamClosed, ...)
	maxSendOffset int64  // absolute watermark the peer has granted us; available credit is maxSendOffset-sentOffset
	sentOffset    int64  // cumulative bytes sent so far
	writeDeadline time.Time
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
	s := &Stream{
		id:            id,
		conn:          conn,
		readBuf:       make([]byte, initial),
		reorder:       make(map[uint32][]byte),
		peerOffset:    int64(initial),
		maxSendOffset: int64(initial),
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

	maxPayload := s.conn.cfg.maxPayload
	for len(p) > 0 {
		chunkLen := len(p)
		if chunkLen > maxPayload {
			chunkLen = maxPayload
		}
		// Never overshoot the window: the receiver's ring is sized exactly
		// to the credit it has granted, so an oversized frame would be
		// dropped and limp in via retransmission.
		for s.maxSendOffset-s.sentOffset < int64(chunkLen) && s.writeErr == nil && !s.writeFIN {
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
		n += len(chunk)
		p = p[len(chunk):]
	}
	if ferr := s.conn.flushSend(); ferr != nil {
		return n, ferr
	}
	return n, nil
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

// discardReadStateLocked drops buffered ring and reorder data. Caller holds readMu.
func (s *Stream) discardReadStateLocked() {
	s.readHead, s.readTail, s.readLen = 0, 0, 0
	s.reorder = make(map[uint32][]byte)
	s.reorderBytes = 0
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

	if s.readCanceled || s.readErr != nil {
		// Read side is done — discard, but ACK so the peer's retransmits
		// stop, and return the discarded bytes as credit so a peer that
		// missed our STOP_SENDING doesn't stall on flow control.
		if len(payload) > 0 {
			s.grantLocked(len(payload))
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
		cp := make([]byte, len(payload))
		copy(cp, payload)
		s.reorder[seq] = cp
		s.reorderBytes += len(payload)
		return true
	}

	// In-order frame.
	if !s.ringFitsLocked(len(payload)) && !s.growLocked(len(payload)) {
		return false // no room; sender will retransmit
	}
	s.ringWrite(payload)
	s.readSeq++
	s.drainReorderLocked()
	s.maybeGrowLocked()
	s.maybeEOFLocked()
	s.readCond.Broadcast()
	return true
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
		s.reorderBytes -= len(data)
		s.readSeq++
		s.readCond.Broadcast()
	}
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

// maybeGrowLocked proactively doubles the ring when it is at least half
// full, granting the delta as credit, so throughput ramps up to the
// configured buffer size without waiting for a stall.
func (s *Stream) maybeGrowLocked() {
	oldCap := len(s.readBuf)
	if s.readLen*2 < oldCap || oldCap >= s.maxBufSize() {
		return
	}
	newCap := oldCap * 2
	if max := s.maxBufSize(); newCap > max {
		newCap = max
	}
	s.reallocRingLocked(newCap)
	s.grantLocked(newCap - oldCap)
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
