//go:build linux

package anadromous

import (
	"io"
	"sync"
	"time"
)

// Stream is a bidirectional byte stream multiplexed over a Connection.
// It implements io.ReadWriteCloser.
type Stream struct {
	id   uint32
	conn *Connection

	// read side
	readMu   sync.Mutex
	readBuf  []byte // ring buffer backing store
	readHead int    // next read position
	readTail int    // next write position
	readLen  int    // buffered bytes count
	readSeq  uint32 // next expected sequence number
	readCond *sync.Cond
	readErr  error // sticky read error (io.EOF, ErrStreamClosed, etc.)

	// write side
	writeMu   sync.Mutex
	writeFIN  bool // true after CloseWrite
	writeSeq  uint32
	writeCond *sync.Cond

	// deadlines
	readDeadline  time.Time
	writeDeadline time.Time

	closed bool
}

func newStream(id uint32, conn *Connection, bufSize int) *Stream {
	s := &Stream{
		id:       id,
		conn:     conn,
		readBuf:  make([]byte, bufSize),
		readSeq:  0,
		writeSeq: 0,
	}
	s.readCond = sync.NewCond(&s.readMu)
	s.writeCond = sync.NewCond(&s.writeMu)
	return s
}

// StreamID returns the stream's identifier.
func (s *Stream) StreamID() uint32 { return s.id }

// Read reads up to len(p) bytes from the stream.
// Blocks until data is available, the stream is closed, or a deadline expires.
func (s *Stream) Read(p []byte) (n int, err error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	for s.readLen == 0 {
		if s.readErr != nil {
			return 0, s.readErr
		}
		if s.closed {
			return 0, ErrStreamClosed
		}
		if !s.readDeadline.IsZero() && time.Now().After(s.readDeadline) {
			return 0, errDeadlineExceeded
		}
		s.readCond.Wait()
	}

	// copy from ring buffer
	n = s.ringRead(p)
	return n, nil
}

// Write writes p to the stream. The data may be split across multiple datagrams.
// Blocks if a write deadline has passed.
func (s *Stream) Write(p []byte) (n int, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.writeFIN {
		return 0, ErrStreamFIN
	}
	if s.closed {
		return 0, ErrStreamClosed
	}
	if !s.writeDeadline.IsZero() && time.Now().After(s.writeDeadline) {
		return 0, errDeadlineExceeded
	}

	// Split into maxPayloadSize chunks and send each as a DATA frame.
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxPayloadSize {
			chunk = chunk[:maxPayloadSize]
		}
		seq := s.writeSeq
		s.writeSeq++

		err = s.conn.sendDataFrame(s.id, seq, chunk)
		if err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// Close closes the stream in both directions.
func (s *Stream) Close() error {
	s.readMu.Lock()
	alreadyClosed := s.closed
	s.closed = true
	if s.readErr == nil {
		s.readErr = ErrStreamClosed
	}
	s.readCond.Broadcast()
	s.readMu.Unlock()

	if alreadyClosed {
		return nil
	}

	s.writeMu.Lock()
	s.writeFIN = true
	s.writeCond.Broadcast()
	s.writeMu.Unlock()

	// Send STREAM_CLOSE frame to peer.
	s.conn.sendControlFrame(frameStreamClose, s.id, 0)
	s.conn.removeStream(s.id)
	return nil
}

// CloseWrite sends a FIN — signals that this side is done writing,
// but the stream remains open for reading.
func (s *Stream) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.writeFIN {
		return nil
	}
	s.writeFIN = true
	return s.conn.sendControlFrame(frameStreamFIN, s.id, s.writeSeq)
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

// deliverData is called when a DATA frame arrives for this stream.
func (s *Stream) deliverData(payload []byte) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	if s.closed || s.readErr != nil {
		return
	}
	s.ringWrite(payload)
	s.readCond.Broadcast()
}

// deliverFIN is called when the remote sends a FIN for this stream.
func (s *Stream) deliverFIN() {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	if s.readErr == nil {
		s.readErr = io.EOF
	}
	s.readCond.Broadcast()
}

// deliverClose is called when the remote closes the stream entirely.
func (s *Stream) deliverClose() {
	s.readMu.Lock()
	if s.readErr == nil {
		s.readErr = io.EOF
	}
	s.closed = true
	s.readCond.Broadcast()
	s.readMu.Unlock()

	s.writeMu.Lock()
	s.writeFIN = true
	s.writeCond.Broadcast()
	s.writeMu.Unlock()
}

// --- ring buffer helpers ---

func (s *Stream) ringWrite(data []byte) {
	cap_ := len(s.readBuf)
	for len(data) > 0 {
		if s.readLen >= cap_ {
			// TODO change to block instead of drop data
			// Buffer full — drop oldest data. Application must read fast enough.
			break
		}
		avail := cap_ - s.readLen
		n := len(data)
		if n > avail {
			n = avail
		}
		// Write in up to two segments (wrap-around).
		end := s.readTail + n
		if end <= cap_ {
			copy(s.readBuf[s.readTail:end], data[:n])
		} else {
			first := cap_ - s.readTail
			copy(s.readBuf[s.readTail:cap_], data[:first])
			copy(s.readBuf[:end-cap_], data[first:n])
		}
		s.readTail = (s.readTail + n) % cap_
		s.readLen += n
		data = data[n:]
	}
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
