//go:build linux

package anadromous

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/tredeske/u/unet"
)

// Connection multiplexes streams over a single UDP socket.
// Both client and server sides use this type once the handshake completes.
type Connection struct {
	cfg    config
	connID uint64

	// underlying UDP socket (unet)
	sock *unet.Socket
	fd   int

	// peer addresses
	remoteAddr unet.Address
	localAddr  unet.Address

	// batched I/O endpoints
	recvEP unet.UdpEndpoint
	sendEP unet.UdpEndpoint

	// receive scratch buffers — one per recvmmsg slot
	recvBufs [][]byte

	// send queue: frames are queued and flushed in batches via sendmmsg
	sendMu   sync.Mutex
	sendBufs [][]byte // pre-allocated datagram buffers
	sendLens []int    // actual length written into each sendBuf
	sendN    int      // number of pending messages in current batch
	iovsPer  int      // iovecs per message slot (2*gsoMaxFrames, or 2 without GSO)

	// UDP GSO (UDP_SEGMENT) packing state, guarded by sendMu. When the
	// socket option is supported, up to gsoMaxFrames full-size DATA frames
	// are packed into one sendmmsg message that the kernel segments into
	// individual datagrams at gsoSize boundaries — amortizing the
	// per-message kernel cost, which profiling shows dominates (>60% CPU)
	// at high throughput. gsoMaxFrames < 2 means GSO is off and every
	// message carries exactly one frame, as before.
	gsoSize      int // segment size == cfg.maxDatagram()
	gsoMaxFrames int
	packIdx      int // message slot currently being packed, -1 when none
	packFrames   int // frames packed into that slot so far
	packBytes    int // total bytes packed into that slot so far
	packHdrOff   int // write cursor into the slot's header-strip buffer

	// stream management
	streamMu      sync.RWMutex
	streams       map[uint32]*Stream
	nextStream    uint32 // next stream ID to allocate
	acceptCh      chan *Stream
	sReadIdsToAck map[uint32][]uint32 // explicit seqs to ACK per stream — dead-stream/reset paths (recvMu)
	sAckDirty     map[uint32]struct{} // live streams with arrivals since the last ACK flush (recvMu)
	ackScratch    []uint32            // reused decode buffer for incoming ACK/NACK frames (recvMu)
	ackSnapBuf    []uint32            // reused snapshot buffer for outgoing ACK frames (recvMu)

	// deadStreams are tombstones for recently removed streams (recvMu).
	// Late retransmits for them are ACKed and discarded instead of
	// resurrecting the stream. Entries expire after tombstoneTTL.
	deadStreams map[uint32]time.Time

	// streamFreedCh is signaled when a stream slot frees up, waking
	// OpenStreamSync waiters.
	streamFreedCh chan struct{}

	// recvMu serializes datagram processing. Normally only readLoop calls
	// handleDatagram, but Listener.readLoop's fallback path can also forward
	// a stray datagram to an established connection from a different
	// goroutine, so the state mutations above need protection.
	recvMu sync.Mutex

	// retransmit tracks unacknowledged DATA frames for fixed-interval,
	// no-backoff retransmission.
	retransmit *retransmitQueue

	// onClose, if set, is invoked once when the connection closes (used by
	// Listener to remove the connection from its tracking map).
	onClose func()

	// established is closed once the handshake completes (client side).
	establishedCh   chan struct{}
	establishedOnce sync.Once

	// connection lifecycle
	closed   atomic.Bool
	closeCh  chan struct{}
	closeErr error
	doneWg   sync.WaitGroup

	// Statuses (accessed from the public API concurrently with readLoop, so
	// these must be atomic rather than plain fields).
	sendPingMs  atomic.Int64
	rttMs       atomic.Int64
	missedPongs int32

	// RTT/RTO estimation (RFC 6298, Jacobson/Karels): srtt/rttvar are only
	// touched by updateRTO, called from handleACK and the PONG handler,
	// both of which already run under recvMu. rtoNs is the published,
	// lock-free view of the current RTO that retransmitLoop and
	// fast-retransmit read from any goroutine — see currentRTO.
	srtt   time.Duration
	rttvar time.Duration
	rtoNs  atomic.Int64

	// statResends counts frames retransmitted over the connection lifetime.
	statResends atomic.Int64
	// statReorder counts frames that arrived out of order (buffered in a
	// stream's reorder map rather than delivered directly).
	statReorder atomic.Int64
	// statFecRecovered counts frames reconstructed from FEC parity instead
	// of retransmission.
	statFecRecovered atomic.Int64

	// role: true if this side initiated the connection (client)
	isClient bool
}

// newConnection creates a Connection around an already-bound unet.Socket.
// The socket must be a UDP socket with NearAddr and FarAddr set.
func newConnection(sock *unet.Socket, fd int, remote unet.Address, connID uint64, isClient bool, cfg config) *Connection {
	c := &Connection{
		cfg:           cfg,
		connID:        connID,
		sock:          sock,
		fd:            fd,
		remoteAddr:    remote,
		streams:       make(map[uint32]*Stream, 64),
		acceptCh:      make(chan *Stream, cfg.maxStreams),
		closeCh:       make(chan struct{}),
		isClient:      isClient,
		retransmit:    newRetransmitQueue(cfg.maxPayload),
		establishedCh: make(chan struct{}),
		deadStreams:   make(map[uint32]time.Time),
		streamFreedCh: make(chan struct{}, 1),
	}
	c.rttMs.Store(-1)
	c.rtoNs.Store(int64(cfg.retransmitTmout))

	// Client-initiated streams use odd IDs (1,3,5,...).
	// Server-initiated streams use even IDs (2,4,6,...).
	if isClient {
		c.nextStream = 1
		c.closed.Store(false)
	} else {
		c.nextStream = 2
		c.closed.Store(false)
		// Server-side connections are usable as soon as they're created
		// (the handshake that created them already proved reachability).
		close(c.establishedCh)
	}

	c.sock.GetNearAddress(&c.localAddr)

	maxDatagram := cfg.maxDatagram()

	// Try to enable UDP GSO: with the socket-level UDP_SEGMENT option set to
	// one datagram, a message of K concatenated full-size frames is
	// segmented by the kernel into K datagrams, so the per-message send
	// cost is amortized across K frames. Messages smaller than one segment
	// (all the control frames) pass through unchanged. The wire format is
	// unaffected — the peer receives ordinary individual datagrams and
	// needs no GSO support of its own.
	c.gsoMaxFrames = 1
	const maxGSOBytes = 60000 // stay under the 64KB IP packet limit
	if frames := maxGSOBytes / maxDatagram; cfg.enableGSO && frames >= 2 {
		// Raw setsockopt rather than sock.SetOptGso: the unet helper closes
		// the socket outright when the option is unsupported (pre-4.18
		// kernel), whereas an unsupported option here should just mean
		// falling back to one frame per message.
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_UDP, unet.UDP_SEGMENT, maxDatagram); err == nil {
			c.gsoSize = maxDatagram
			c.gsoMaxFrames = frames
			if c.gsoMaxFrames > 8 {
				c.gsoMaxFrames = 8
			}
		}
	}
	// Try to enable UDP GRO, the receive-side twin: the kernel hands over a
	// burst of equal-size datagrams from the same sender as ONE coalesced
	// buffer, so a GSO burst from the peer costs one recvmmsg slot instead
	// of one per datagram. Frames are self-describing (header carries the
	// payload length), so the coalesced buffer is simply parsed as a frame
	// sequence — see handleDatagramLocked — and no cmsg plumbing is needed.
	// Without GRO each buffer holds exactly one frame and the same parse
	// loop runs a single iteration. GSO without GRO on the receiver is a
	// LOSS, not a wash: the peer's bursts arrive as small back-to-back
	// wakeups that shrink the receiver's effective recvmmsg batch size, so
	// only pack frames when the coalescing side is on too.
	recvBufLen := maxDatagram
	const udpGRO = 104 // UDP_GRO socket option (Linux 5.0+)
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_UDP, udpGRO, 1); err == nil {
		recvBufLen = 65536
	} else {
		c.gsoSize = 0
		c.gsoMaxFrames = 1
	}
	c.packIdx = -1
	c.iovsPer = 2 * c.gsoMaxFrames

	// Set up batched receive endpoint.
	c.sReadIdsToAck = make(map[uint32][]uint32)
	c.sAckDirty = make(map[uint32]struct{})
	c.recvBufs = make([][]byte, cfg.batchSize)
	recvIdx := 0
	c.recvEP.SetupVectors(cfg.batchSize, 1, func(iov []syscall.Iovec) {
		b := make([]byte, recvBufLen)
		c.recvBufs[recvIdx] = b
		iov[0].Base = &b[0]
		iov[0].Len = uint64(recvBufLen)
		recvIdx++
	}, nil) // connected socket, no name needed

	// Set up batched send endpoint. Each message slot has iovsPer iovecs
	// used in (header, payload) pairs: iov[0] is the slot's own buffer,
	// holding either a fully-encoded frame (header and payload combined,
	// for the low-traffic control frame types) or one or more 13-byte
	// headers (for DATA/FIN/RESET, whose payloads live in the retransmit
	// arena); each odd iovec points directly at an arena payload so bulk
	// data is never copied into a send buffer at all (see
	// commitSendSlotZeroCopy and queueFrameGSOLocked).
	c.sendBufs = make([][]byte, cfg.batchSize)
	c.sendLens = make([]int, cfg.batchSize)
	sendIdx := 0
	c.sendEP.SetupVectors(cfg.batchSize, c.iovsPer, func(iov []syscall.Iovec) {
		b := make([]byte, maxDatagram)
		c.sendBufs[sendIdx] = b
		iov[0].Base = &b[0]
		iov[0].Len = 0 // set to actual frame size on each send
		for i := 1; i < len(iov); i++ {
			iov[i].Base = nil
			iov[i].Len = 0
		}
		sendIdx++
	}, nil) // connected socket, no name needed

	return c
}

// start begins the read loop and the background retransmit/keepalive loops.
func (c *Connection) start() {
	c.doneWg.Add(1)
	go c.readLoop()

	c.doneWg.Add(1)
	go c.retransmitLoop()

	if c.cfg.keepAlive > 0 {
		c.doneWg.Add(1)
		go c.keepAliveLoop()
	}
}

// ConnID returns the connection identifier agreed during handshake.
func (c *Connection) ConnID() uint64 { return c.connID }

// LocalAddr returns the local network address.
func (c *Connection) LocalAddr() net.Addr { return &c.localAddr }

// RemoteAddr returns the remote network address.
func (c *Connection) RemoteAddr() net.Addr { return &c.remoteAddr }

// OpenStream creates a new outbound stream. Stream opens are implicit: the
// peer learns of the stream when its first frame arrives, so nothing is sent
// on the wire here and opens cannot be lost. Returns ErrMaxStreams when at
// the concurrent stream limit (see OpenStreamSync to wait instead).
func (c *Connection) OpenStream(ctx context.Context) (*Stream, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}

	c.streamMu.Lock()
	if len(c.streams) >= c.cfg.maxStreams {
		c.streamMu.Unlock()
		return nil, ErrMaxStreams
	}
	id := c.nextStream
	c.nextStream += 2
	s := newStream(id, c, c.cfg.streamBufSize)
	c.streams[id] = s
	c.streamMu.Unlock()

	return s, nil
}

// OpenStreamSync creates a new outbound stream, blocking until a stream slot
// is available, the context is done, or the connection closes.
func (c *Connection) OpenStreamSync(ctx context.Context) (*Stream, error) {
	for {
		s, err := c.OpenStream(ctx)
		if err != ErrMaxStreams {
			return s, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closeCh:
			return nil, ErrClosed
		case <-c.streamFreedCh:
		}
	}
}

// AcceptStream waits for the remote side to open a stream.
func (c *Connection) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeCh:
		return nil, ErrClosed
	case s := <-c.acceptCh:
		return s, nil
	}
}

// CloseWithError closes the connection and all streams. The error code and
// reason are accepted for quic-go API compatibility; they are not currently
// transmitted to the peer.
func (c *Connection) CloseWithError(code uint64, reason string) error {
	return c.Close()
}

// Close closes the connection and all streams.
func (c *Connection) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	if c.onClose != nil {
		c.onClose()
	}

	// Send GOAWAY to peer (best-effort). The socket is still open at this
	// point (Shutdown happens below), so this reaches flushSendLocked fine.
	c.sendControlFrame(frameGoAway, 0, 0)

	close(c.closeCh)

	// Close all streams.
	c.streamMu.Lock()
	for _, s := range c.streams {
		s.deliverClose()
	}
	c.streams = nil
	c.streamMu.Unlock()

	// Shutdown the socket to unblock the read loop.
	c.sock.Shutdown()

	// Wait for the read loop to exit.
	c.doneWg.Wait()

	// Close the socket fd.
	c.sock.Close()
	return nil
}

// --- send path (batched via sendmmsg) ---

// frameHdrLen returns the wire header length for a frame type: the fixed
// header, plus the FEC metadata prefix for DATA frames when FEC is on (the
// prefix rides in the header iovec on the zero-copy paths, so the payload
// can still be referenced from the retransmit arena unmodified).
func (c *Connection) frameHdrLen(ftype uint8) int {
	if ftype == frameData && c.cfg.fecGroup > 0 {
		return frameHeaderSize + fecMetaLen
	}
	return frameHeaderSize
}

// writeFrameHdr writes a frame's header (and, for DATA under FEC, its
// zeroed metadata prefix) into buf, returning the header length. The wire
// length field covers the prefix plus dataLen.
func (c *Connection) writeFrameHdr(buf []byte, ftype uint8, streamID, seq uint32, dataLen int) int {
	hdrLen := c.frameHdrLen(ftype)
	encodeHeader(buf, ftype, streamID, seq, uint32(hdrLen-frameHeaderSize+dataLen))
	for i := frameHeaderSize; i < hdrLen; i++ {
		buf[i] = 0
	}
	return hdrLen
}

// sendDataFrame queues a DATA frame and records it for retransmission until
// acknowledged. payload is copied once into the retransmit arena (see
// retransmitQueue.add) and referenced directly on the wire from there — see
// commitSendSlotZeroCopy — rather than copied again into a send buffer. It
// does not flush; callers that want the frame on the wire immediately (or
// after queuing several) must call flushSend().
func (c *Connection) sendDataFrame(streamID, seq uint32, payload []byte) error {
	data := c.retransmit.add(frameData, streamID, seq, payload)
	if c.gsoMaxFrames >= 2 {
		c.sendMu.Lock()
		err := c.queueFrameGSOLocked(frameData, streamID, seq, data)
		c.sendMu.Unlock()
		return err
	}
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	hdrLen := c.writeFrameHdr(buf, frameData, streamID, seq, len(data))
	return c.commitSendSlotZeroCopy(idx, hdrLen, data)
}

// sendFecFrame sends a parity frame covering count DATA frames of group k
// (seqs [k*G, k*G+count)). Unreliable by design: parity is itself
// redundancy, so a lost parity frame just means that group has no FEC
// protection and recovery falls back to NACK/timeout. xorData is copied
// into the send slot's own buffer (it's the stream's live accumulator).
func (c *Connection) sendFecFrame(streamID, group uint32, count, xorLen int, xorData []byte) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	meta := uint32(count)<<16 | uint32(xorLen)
	encodeHeader(buf, frameFEC, streamID, group, uint32(fecMetaLen+len(xorData)))
	binary.BigEndian.PutUint32(buf[frameHeaderSize:], meta)
	copy(buf[frameHeaderSize+fecMetaLen:], xorData)
	return c.commitSendSlot(idx, frameHeaderSize+fecMetaLen+len(xorData))
}

// sendReliableFrame sends a frame immediately and retransmits it until the
// peer acknowledges its (streamID, seq). Used for FIN and RESET frames,
// which occupy the sequence position after the stream's last data frame.
// Like sendDataFrame, payload is copied once into the retransmit arena and
// referenced directly on the wire rather than copied again.
func (c *Connection) sendReliableFrame(ftype uint8, streamID, seq uint32, payload []byte) error {
	data := c.retransmit.add(ftype, streamID, seq, payload)
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	hdrLen := c.writeFrameHdr(buf, ftype, streamID, seq, len(data))
	if err := c.commitSendSlotZeroCopy(idx, hdrLen, data); err != nil {
		return err
	}
	return c.flushSend()
}

// sendWindowUpdate informs the peer of the absolute (cumulative) offset it
// is now allowed to send up to on a stream. Sent unreliably and never
// retransmitted — see windowUpdatePayload for why that's safe.
func (c *Connection) sendWindowUpdate(streamID uint32, offset uint64) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeFrame(buf, frameWindowUpdate, streamID, 0, windowUpdatePayload(offset))
	if err := c.commitSendSlot(idx, n); err != nil {
		return err
	}
	return c.flushSend()
}

// sendControlFrame sends a control frame (no payload) immediately.
func (c *Connection) sendControlFrame(ftype uint8, streamID, seq uint32) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	encodeHeader(buf, ftype, streamID, seq, 0)
	if err := c.commitSendSlot(idx, frameHeaderSize); err != nil {
		return err
	}
	return c.flushSend()
}

// sendACKFrame sends an ACK frame immediately.
// len(seqs) must not exceed maxAckSeqsPerFrame.
func (c *Connection) sendACKFrame(streamID, cumulative uint32, seqs []uint32) error {
	if err := c.queueACKFrame(streamID, cumulative, seqs); err != nil {
		return err
	}
	return c.flushSend()
}

// sendNackFrame asks the peer to immediately resend the listed frames of a
// stream (fast retransmit). Same seq-list wire format as ACK.
// len(seqs) must not exceed maxAckSeqsPerFrame.
func (c *Connection) sendNackFrame(streamID uint32, seqs []uint32) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeSeqListFrame(buf, frameNack, streamID, 0, seqs)
	if err := c.commitSendSlot(idx, n); err != nil {
		return err
	}
	return c.flushSend()
}

// queueResendFrame queues one pending frame for retransmission, zero-copy
// from the retransmit arena and GSO-packed with other resends when packing
// is on. Callers flush after queuing a batch.
func (c *Connection) queueResendFrame(e retransmitEntry) error {
	if c.gsoMaxFrames >= 2 {
		c.sendMu.Lock()
		err := c.queueFrameGSOLocked(e.ftype, e.streamID, e.seq, e.data)
		c.sendMu.Unlock()
		return err
	}
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	hdrLen := c.writeFrameHdr(buf, e.ftype, e.streamID, e.seq, len(e.data))
	return c.commitSendSlotZeroCopy(idx, hdrLen, e.data)
}

// queueACKFrame queues an ACK frame without flushing, so a flush of many
// ACK frames (one per stream per receive batch — see
// flushPendingAcksLocked) costs one sendmmsg instead of one per frame.
// len(seqs) must not exceed maxAckSeqsPerFrame.
func (c *Connection) queueACKFrame(streamID, cumulative uint32, seqs []uint32) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeAckFrame(buf, streamID, cumulative, seqs)
	return c.commitSendSlot(idx, n)
}

// acquireSendSlot reserves a message slot in the send batch. If the batch is
// full, it flushes first. Any GSO message being packed is closed so this
// frame gets its own slot after it, preserving queue order.
func (c *Connection) acquireSendSlot() (buf []byte, idx int, err error) {
	c.sendMu.Lock()
	c.closePackLocked()
	if c.sendN >= c.cfg.batchSize {
		err = c.flushSendLocked()
		if err != nil {
			c.sendMu.Unlock()
			return nil, 0, err
		}
	}
	idx = c.sendN
	buf = c.sendBufs[idx]
	return buf, idx, nil
}

// commitSendSlot finishes writing a fully-encoded frame (header and payload
// both already written into the slot's own buffer) to a send slot, flushing
// only if the batch is now full. Caller holds sendMu (acquired by
// acquireSendSlot) and this releases it.
func (c *Connection) commitSendSlot(idx int, n int) error {
	base := idx * c.iovsPer
	c.sendEP.Iov[base].Len = uint64(n)
	c.sendEP.Hdrs[idx].Iovlen = 1
	c.sendEP.Hdrs[idx].NTransferred = 0
	c.sendLens[idx] = n
	c.sendN = idx + 1

	var err error
	if c.sendN >= c.cfg.batchSize {
		err = c.flushSendLocked()
	}
	c.sendMu.Unlock()
	return err
}

// commitSendSlotZeroCopy finishes a slot whose header has already been
// written (via encodeHeader) into the first frameHeaderSize bytes of its own
// buffer, and points the slot's second iovec directly at payload instead of
// copying payload in — the kernel gathers the two iovecs into one datagram
// during sendmmsg, so payload is sent by reference. This is only safe
// because payload is always an arena-owned buffer that retransmitQueue
// guarantees won't be reused until drainPendingFree runs, which this
// function's caller (via flushSendLocked) only does after the batch
// referencing it has already been handed to the kernel. Caller holds sendMu
// (acquired by acquireSendSlot) and this releases it.
func (c *Connection) commitSendSlotZeroCopy(idx, hdrLen int, payload []byte) error {
	base := idx * c.iovsPer
	c.sendEP.Iov[base].Len = uint64(hdrLen)
	if len(payload) > 0 {
		c.sendEP.Iov[base+1].Base = &payload[0]
	} else {
		c.sendEP.Iov[base+1].Base = nil
	}
	c.sendEP.Iov[base+1].Len = uint64(len(payload))
	c.sendEP.Hdrs[idx].Iovlen = 2
	c.sendEP.Hdrs[idx].NTransferred = 0
	c.sendLens[idx] = hdrLen + len(payload)
	c.sendN = idx + 1

	var err error
	if c.sendN >= c.cfg.batchSize {
		err = c.flushSendLocked()
	}
	c.sendMu.Unlock()
	return err
}

// queueFrameGSOLocked queues a reliable frame into the GSO message currently
// being packed, opening a new one if needed. The message accumulates
// (header, payload) iovec pairs — payloads referenced zero-copy from the
// retransmit arena — and is closed when it reaches gsoMaxFrames or when a
// short (non-full) frame arrives, since GSO requires every segment except
// the last to be exactly gsoSize. Caller holds sendMu; only call when
// gsoMaxFrames >= 2.
func (c *Connection) queueFrameGSOLocked(ftype uint8, streamID, seq uint32, payload []byte) error {
	if c.packIdx < 0 {
		if c.sendN >= c.cfg.batchSize {
			if err := c.flushSendLocked(); err != nil {
				return err
			}
		}
		// Reserve the slot up front so interleaved single-frame sends and
		// flushes account for it correctly.
		c.packIdx = c.sendN
		c.sendN++
		c.packFrames = 0
		c.packBytes = 0
		c.packHdrOff = 0
	}
	idx := c.packIdx
	hdrBuf := c.sendBufs[idx][c.packHdrOff:]
	hdrLen := c.writeFrameHdr(hdrBuf, ftype, streamID, seq, len(payload))
	base := idx*c.iovsPer + 2*c.packFrames
	c.sendEP.Iov[base].Base = &hdrBuf[0]
	c.sendEP.Iov[base].Len = uint64(hdrLen)
	if len(payload) > 0 {
		c.sendEP.Iov[base+1].Base = &payload[0]
	} else {
		c.sendEP.Iov[base+1].Base = nil
	}
	c.sendEP.Iov[base+1].Len = uint64(len(payload))
	c.packFrames++
	c.packHdrOff += hdrLen
	c.packBytes += hdrLen + len(payload)

	full := hdrLen+len(payload) >= c.gsoSize
	if c.packFrames >= c.gsoMaxFrames || !full {
		c.closePackLocked()
		if c.sendN >= c.cfg.batchSize {
			return c.flushSendLocked()
		}
	}
	return nil
}

// closePackLocked finalizes the GSO message being packed, if any: its iovec
// count and length are stamped so flushSendLocked can hand it to the kernel.
// Caller holds sendMu.
func (c *Connection) closePackLocked() {
	if c.packIdx < 0 {
		return
	}
	idx := c.packIdx
	c.sendEP.Hdrs[idx].Iovlen = uint64(2 * c.packFrames)
	c.sendEP.Hdrs[idx].NTransferred = 0
	c.sendLens[idx] = c.packBytes
	c.packIdx = -1
}

// flushSend pushes any queued-but-unflushed datagrams to the wire in a
// single sendmmsg call.
func (c *Connection) flushSend() error {
	c.sendMu.Lock()
	err := c.flushSendLocked()
	c.sendMu.Unlock()
	return err
}

// flushSendLocked sends all queued messages via sendmmsg. Caller holds sendMu.
func (c *Connection) flushSendLocked() error {
	c.closePackLocked()
	if c.sendN == 0 {
		return nil
	}
	if c.sock.IsShutdown() {
		c.sendN = 0
		return ErrClosed
	}

	n := c.sendN
	_, errno := unet.SendMMsgRetry(uintptr(c.fd), c.sendEP.Hdrs[:n], n)
	c.sendN = 0
	// No per-iovec reset is needed: every path that claims a slot stamps
	// its Iovlen, lengths, and iovec pointers before the next flush.

	// The batch (and every zero-copy arena pointer in it) has now been
	// handed to the kernel, so any arena buffers freed by ACKs/purges while
	// it was pending are safe to recycle — see retransmitQueue.drainPendingFree.
	c.retransmit.drainPendingFree()

	if errno != 0 {
		return errno
	}
	return nil
}

// --- background loops ---

// retransmitLoop periodically resends any reliable frame (DATA, FIN, RESET)
// that has been outstanding for longer than the configured (fixed,
// non-backing-off) retransmit timeout, fails streams that exceed the retry
// budget, garbage-collects finished streams, and expires tombstones.
func (c *Connection) retransmitLoop() {
	defer c.doneWg.Done()

	const scanInterval = 20 * time.Millisecond
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case <-ticker.C:
		}

		resend := c.retransmit.due(c.currentRTO())

		for _, e := range resend {
			if c.queueResendFrame(e) != nil {
				break // connection is going away
			}
		}
		if len(resend) > 0 {
			c.statResends.Add(int64(len(resend)))
			c.flushSend()
		}

		c.gcStreams()
	}
}

// gcStreams removes streams whose both directions have finished and whose
// outgoing frames are all acknowledged, and expires old tombstones.
func (c *Connection) gcStreams() {
	var done []uint32
	c.streamMu.RLock()
	for id, s := range c.streams {
		if s.finished() && !c.retransmit.hasStream(id) {
			done = append(done, id)
		}
	}
	c.streamMu.RUnlock()

	for _, id := range done {
		c.removeStream(id)
	}

	c.recvMu.Lock()
	now := time.Now()
	for id, t := range c.deadStreams {
		if now.Sub(t) > tombstoneTTL {
			delete(c.deadStreams, id)
		}
	}
	c.recvMu.Unlock()
}

// keepAliveLoop sends periodic pings and closes the connection if the peer
// stops responding, so a vanished peer's connection state (and the
// Listener's tracking entry) doesn't leak forever.
func (c *Connection) keepAliveLoop() {
	defer c.doneWg.Done()

	const maxMissedPongs = 3
	ticker := time.NewTicker(c.cfg.keepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case <-ticker.C:
		}

		if atomic.AddInt32(&c.missedPongs, 1) > maxMissedPongs {
			go c.Close()
			return
		}
		c.SendPing(0, 0)
	}
}

// --- receive path (batched via recvmmsg) ---

func (c *Connection) readLoop() {
	defer c.doneWg.Done()

	for {
		if c.closed.Load() {
			return
		}

		// Reset iov lens for receive.
		for i := 0; i < c.cfg.batchSize; i++ {
			c.recvEP.Iov[i].Base = &c.recvBufs[i][0]
			c.recvEP.Iov[i].Len = uint64(len(c.recvBufs[i]))
			c.recvEP.Hdrs[i].NTransferred = 0
		}

		messages, errno := c.recvEP.RecvMMsg(c.fd)
		if errno != 0 {
			if errno == syscall.EINTR {
				continue
			}
			// Socket was shut down or error — exit loop.
			return
		}
		if messages == 0 {
			// recvmmsg returned 0 messages — check if disabled.
			if c.sock.IsShutdown() {
				return
			}
			continue
		}

		// One recvMu acquisition per recvmmsg batch rather than per
		// datagram: at high frame rates the per-datagram lock/unlock cycle
		// (batchSize × 2 atomic ops per syscall) is measurable, and nothing
		// inside the dispatch path blocks for long.
		c.recvMu.Lock()
		for i := 0; i < messages; i++ {
			nbytes := int(c.recvEP.Hdrs[i].NTransferred)
			if nbytes < frameHeaderSize {
				continue // too small, discard
			}
			c.handleDatagramLocked(c.recvBufs[i][:nbytes])
		}
		c.flushPendingAcksLocked()
		c.recvMu.Unlock()
	}
}

// handleDatagram processes a single received datagram. Safe to call from
// any goroutine — used by Listener.readLoop's fallback path; the connection's
// own readLoop batches the lock across a whole recvmmsg batch instead.
func (c *Connection) handleDatagram(buf []byte) {
	c.recvMu.Lock()
	c.handleDatagramLocked(buf)
	c.recvMu.Unlock()
}

// handleDatagramLocked processes a received buffer. With UDP_GRO active the
// kernel may deliver several coalesced datagrams in one buffer, so this
// parses it as a sequence of frames — each frame's header carries its
// payload length, so consecutive frames are self-delimiting; without GRO
// the buffer holds exactly one frame and the loop runs once. A malformed
// frame discards the remainder of the buffer (frame boundaries after it
// can't be trusted). Caller holds recvMu.
func (c *Connection) handleDatagramLocked(buf []byte) {
	for len(buf) >= frameHeaderSize {
		f, err := decodeFrame(buf)
		if err != nil {
			return // discard malformed remainder
		}
		c.dispatchFrameLocked(f)
		buf = buf[frameHeaderSize+int(f.length):]
	}
}

// dispatchFrameLocked routes one decoded frame. Caller holds recvMu.
func (c *Connection) dispatchFrameLocked(f frame) {
	// Any successfully decoded frame from the peer is proof of life: reset
	// the idle/keepalive counter here rather than only on framePong. Ping is
	// a tiny, unreliable, single-packet control frame like any other, so
	// under loss it (or its reply) can be dropped several times in a row
	// even while the connection is actively exchanging bulk DATA/ACK traffic
	// — that's not an idle peer, and treating it as one contradicts
	// WithIdleTimeout's documented "equivalent to quic-go's MaxIdleTimeout"
	// behavior, where the idle timer resets on any received packet.
	atomic.StoreInt32(&c.missedPongs, 0)

	switch f.ftype {
	case frameData:
		if c.cfg.fecGroup > 0 {
			if len(f.payload) < fecMetaLen {
				return // malformed under FEC framing
			}
			f.payload = f.payload[fecMetaLen:] // strip the (reserved) FEC meta prefix
		}
		c.handleData(f)
	case frameFEC:
		c.handleFec(f)
	case frameStreamFIN:
		c.handleStreamFIN(f)
	case frameStreamReset:
		c.handleStreamReset(f)
	case frameStopSending:
		c.handleStopSending(f)
	case frameWindowUpdate:
		c.handleWindowUpdate(f)
	case framePing:
		c.sendControlFrame(framePong, f.streamID, f.seq)
	case framePong:
		if sentAt := c.sendPingMs.Load(); sentAt > 0 {
			rttMs := time.Now().UnixMilli() - sentAt
			c.rttMs.Store(rttMs)
			c.sendPingMs.Store(0)
			c.updateRTO(time.Duration(rttMs) * time.Millisecond)
		}
	case frameGoAway:
		go c.Close()
	case frameACK:
		c.handleACK(f)
	case frameNack:
		c.handleNack(f)
	case frameHandshake:
		c.handleHandshake(f)
	default:
		// unknown frame type (including the legacy explicit stream
		// open/close types), ignore
	}
}

// flushPendingAcks sends any accumulated ACKs. Locking wrapper for callers
// outside the connection's own readLoop (Listener fallback path).
func (c *Connection) flushPendingAcks() {
	c.recvMu.Lock()
	c.flushPendingAcksLocked()
	c.recvMu.Unlock()
}

// flushPendingAcksLocked sends any accumulated ACKs, chunked to fit the wire
// format. Called after each receive batch, holding recvMu.
//
// Live streams marked dirty this batch get a snapshot ACK — their full
// receive state (cumulative watermark + out-of-order seqs), idempotent and
// re-advertised every flush so lost ACK frames self-heal. Explicit seq
// lists (cumulative=0) remain for the paths with no live stream state to
// snapshot: dead-stream/tombstone acking and RESET/late-FIN handling. The
// per-stream slices/buffers are truncated in place and retained so
// steady-state flushing allocates nothing; removeStream deletes a stream's
// slots when it goes away.
func (c *Connection) flushPendingAcksLocked() {
	maxAcks := maxAckSeqsPerFrame(c.cfg.maxPayload)
	queued := false

	for streamID := range c.sAckDirty {
		c.streamMu.RLock()
		s := c.streams[streamID]
		c.streamMu.RUnlock()
		if s != nil {
			cum, seqs := s.ackSnapshot(c.ackSnapBuf, maxAcks)
			c.ackSnapBuf = seqs[:0]
			c.queueACKFrame(streamID, cum, seqs)
			queued = true
		}
		delete(c.sAckDirty, streamID)
	}

	for streamID, seqs := range c.sReadIdsToAck {
		if len(seqs) == 0 {
			continue
		}
		rest := seqs
		for len(rest) > 0 {
			n := len(rest)
			if n > maxAcks {
				n = maxAcks
			}
			c.queueACKFrame(streamID, 0, rest[:n])
			rest = rest[n:]
		}
		queued = true
		c.sReadIdsToAck[streamID] = seqs[:0]
	}
	if queued {
		c.flushSend()
	}
}

// queueAck records an explicit (stream, seq) for the next ACK flush, for
// frames with no live stream to snapshot. Caller holds recvMu.
func (c *Connection) queueAck(streamID, seq uint32) {
	c.sReadIdsToAck[streamID] = append(c.sReadIdsToAck[streamID], seq)
}

// markAckDirty schedules a live stream for a snapshot ACK at the next
// flush. Caller holds recvMu.
func (c *Connection) markAckDirty(streamID uint32) {
	c.sAckDirty[streamID] = struct{}{}
}

// streamForFrame resolves the stream a DATA/FIN frame belongs to, creating
// it when the frame is the first sign of a new peer-initiated stream
// (stream opens are implicit). Caller holds recvMu.
//
// Returns (nil, true) when the frame should be ACKed and discarded (stream
// recently finished, or a stale frame for one of our own old streams), and
// (nil, false) when it should be dropped without an ACK.
func (c *Connection) streamForFrame(streamID uint32) (s *Stream, ackDiscard bool) {
	c.streamMu.RLock()
	s, ok := c.streams[streamID]
	c.streamMu.RUnlock()
	if ok {
		return s, false
	}

	if _, dead := c.deadStreams[streamID]; dead {
		return nil, true // late retransmit for a finished stream
	}

	// Client-initiated streams are odd, server-initiated even. A frame for
	// an unknown stream with OUR parity is stale (our stream is long gone,
	// tombstone expired) — ACK it so the peer stops retransmitting.
	peerInitiated := (streamID%2 == 1) != c.isClient
	if !peerInitiated {
		return nil, true
	}

	// First frame of a new peer-initiated stream: create and hand to Accept.
	c.streamMu.Lock()
	if len(c.streams) >= c.cfg.maxStreams {
		c.streamMu.Unlock()
		// Refuse: reset both directions so the opener fails fast instead
		// of retransmitting into the void.
		c.sendControlFrame(frameStopSending, streamID, 0)
		c.sendResetNoQueue(streamID, 0)
		return nil, false
	}
	s = newStream(streamID, c, c.cfg.streamBufSize)
	c.streams[streamID] = s
	c.streamMu.Unlock()

	select {
	case c.acceptCh <- s:
	default:
		// Accept channel full — stream exists but was not delivered.
	}
	return s, false
}

// sendResetNoQueue fires a RESET frame without retransmit tracking. Used to
// refuse streams we have no local state for.
func (c *Connection) sendResetNoQueue(streamID uint32, code uint64) {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return
	}
	n := encodeFrame(buf, frameStreamReset, streamID, 0, resetPayload(code))
	if c.commitSendSlot(idx, n) == nil {
		c.flushSend()
	}
}

func (c *Connection) handleData(f frame) {
	s, ackDiscard := c.streamForFrame(f.streamID)
	if s == nil {
		if ackDiscard {
			c.queueAck(f.streamID, f.seq)
		}
		return
	}
	// Mark for a snapshot ACK regardless of deliver's verdict: duplicates
	// mean the peer is retransmitting something our snapshot already covers
	// — evidence a previous ACK was lost — so re-advertising the snapshot
	// is exactly the right response.
	s.deliver(f.seq, f.payload, false)
	c.markAckDirty(f.streamID)
}

func (c *Connection) handleStreamFIN(f frame) {
	s, ackDiscard := c.streamForFrame(f.streamID)
	if s == nil {
		if ackDiscard {
			c.queueAck(f.streamID, f.seq)
		}
		return
	}
	s.deliver(f.seq, nil, true)
	c.markAckDirty(f.streamID)
}

// handleFec routes a parity frame to its stream for possible zero-RTT loss
// recovery (see Stream.applyFec). Parity for unknown/dead streams is
// silently dropped — it's pure redundancy. Caller holds recvMu.
func (c *Connection) handleFec(f frame) {
	if c.cfg.fecGroup == 0 || len(f.payload) < fecMetaLen {
		return
	}
	meta := binary.BigEndian.Uint32(f.payload[:fecMetaLen])
	count := int(meta >> 16)
	xorLen := int(meta & 0xffff)
	if count <= 0 || count > c.cfg.fecGroup {
		return // malformed or mismatched config
	}
	c.streamMu.RLock()
	s := c.streams[f.streamID]
	c.streamMu.RUnlock()
	if s == nil {
		return
	}
	if s.applyFec(f.seq, count, xorLen, f.payload[fecMetaLen:]) {
		c.statFecRecovered.Add(1)
		// The reconstruction advanced receive state: advertise it.
		c.markAckDirty(f.streamID)
	}
}

// handleStreamReset processes a peer's write-side abort. Resets are
// retransmitted by the peer until ACKed, so always queue the ACK.
func (c *Connection) handleStreamReset(f frame) {
	c.queueAck(f.streamID, f.seq)

	c.streamMu.RLock()
	s, ok := c.streams[f.streamID]
	c.streamMu.RUnlock()
	if ok {
		s.deliverReset(decodeResetPayload(f.payload))
	}
}

// handleStopSending processes the peer's request that we stop sending on a
// stream: cancel our write side and reset back.
func (c *Connection) handleStopSending(f frame) {
	c.streamMu.RLock()
	s, ok := c.streams[f.streamID]
	c.streamMu.RUnlock()
	if ok {
		s.onStopSending(uint64(f.seq))
	}
}

// handleWindowUpdate applies an absolute send watermark to a stream.
func (c *Connection) handleWindowUpdate(f frame) {
	offset, err := decodeWindowUpdatePayload(f.payload)
	if err != nil {
		return // discard malformed window update
	}
	c.streamMu.RLock()
	s, ok := c.streams[f.streamID]
	c.streamMu.RUnlock()
	if ok {
		s.setMaxSendOffset(int64(offset))
	}
}

// handleHandshake processes a handshake-ack frame arriving on an already
// constructed Connection. The initial handshake (which creates the
// Connection in the first place) is parsed by Listener.readLoop directly.
//
// On the client, this is the peer's ack: mark the connection established.
// On the server, this is a retried handshake from a client that never saw
// our first ack (lost in transit) — just re-send the ack. This is the whole
// retry mechanism for the handshake: fixed-interval resend by the client,
// idempotent echo by the server, no timers on the server side at all.
func (c *Connection) handleHandshake(f frame) {
	if c.closed.Load() {
		return
	}
	if c.isClient {
		c.establishedOnce.Do(func() { close(c.establishedCh) })
		return
	}
	c.sendControlFrame(frameHandshake, 0, 0)
}

func (c *Connection) handleACK(f frame) {
	cum, seqs, err := decodeSeqListFrame(f, c.ackScratch)
	if err != nil {
		return // discard malformed ACK
	}
	c.ackScratch = seqs[:0] // retain capacity for the next ACK frame
	freedCum, sampleCum := c.retransmit.ackCumulative(f.streamID, cum)
	freedList, sampleList := c.retransmit.ackMany(f.streamID, seqs)
	sample := sampleCum
	if sample == 0 || (sampleList > 0 && sampleList < sample) {
		sample = sampleList
	}
	if sample > 0 {
		c.updateRTO(sample)
	}
	freed := freedCum + freedList
	if freed == 0 {
		return
	}
	c.streamMu.RLock()
	s, ok := c.streams[f.streamID]
	c.streamMu.RUnlock()
	if ok {
		s.creditAcked(freed)
	}
}

// handleNack immediately resends the (stream, seq...) frames the peer says
// are missing, bypassing the retransmit timer. This is the fast-retransmit
// path: the peer only NACKs once it has direct evidence of loss (later
// frames arrived and the gap persisted past the reorder grace), so recovery
// happens in about one RTT instead of waiting out the timer. Seqs that are
// already acknowledged or unknown are silently skipped.
func (c *Connection) handleNack(f frame) {
	_, seqs, err := decodeSeqListFrame(f, c.ackScratch)
	if err != nil {
		return // discard malformed NACK
	}
	c.ackScratch = seqs[:0] // retain capacity (shared with handleACK; both run under recvMu)
	resent := 0
	for _, seq := range seqs {
		e, ok := c.retransmit.getForResend(f.streamID, seq)
		if !ok {
			continue
		}
		if c.queueResendFrame(e) != nil {
			break // connection is going away
		}
		resent++
	}
	if resent > 0 {
		c.statResends.Add(int64(resent))
		c.flushSend()
	}
}

// tombstoneTTL is how long a removed stream's ID keeps ACKing late
// retransmits before the tombstone expires. Comfortably longer than the
// worst-case retransmit horizon.
const tombstoneTTL = 10 * time.Second

// removeStream removes a stream from the connection, leaving a tombstone so
// late retransmits are ACKed rather than resurrecting the stream, and wakes
// any OpenStreamSync waiter.
func (c *Connection) removeStream(id uint32) {
	c.streamMu.Lock()
	delete(c.streams, id)
	c.streamMu.Unlock()

	c.retransmit.purgeStream(id)

	c.recvMu.Lock()
	c.deadStreams[id] = time.Now()
	// Drop the stream's retained ACK-accumulation slots (kept alive across
	// flushes for reuse — see flushPendingAcksLocked).
	delete(c.sReadIdsToAck, id)
	delete(c.sAckDirty, id)
	c.recvMu.Unlock()

	select {
	case c.streamFreedCh <- struct{}{}:
	default:
	}
}

// bindToDevice returns a unet socket option that binds the socket to a
// network interface via SO_BINDTODEVICE. Requires CAP_NET_RAW or root.
func bindToDevice(ifname string) unet.SockOpt {
	return func(s *unet.Socket) error {
		fd, ok := s.Fd.Get()
		if !ok {
			return ErrClosed
		}
		return syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifname)
	}
}

// --- Dial (client entry point) ---

// Dial establishes a new Connection to the given address.
// addr should be "host:port".
func Dial(ctx context.Context, addr string, opts ...Option) (*Connection, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	host, port, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}

	sock := unet.NewSocket().
		ResolveFarAddr(host, port).
		ResolveNearAddr("0.0.0.0", 0).
		ConstructUdp().
		SetOptRcvBuf(cfg.recvBufSize).
		SetOptSndBuf(cfg.sendBufSize).
		SetOpt(bindToDevice(cfg.bindDevice), cfg.bindDevice == "")

	if cfg.socketOpts != nil {
		cfg.socketOpts(sock)
	}

	sock = sock.Bind().Connect()
	sock, err = sock.Done()
	if err != nil {
		return nil, err
	}

	fd, valid := sock.Fd.Get()
	if !valid {
		sock.Close()
		return nil, ErrClosed
	}

	var remote unet.Address
	sock.GetFarAddress(&remote)

	connID := generateConnID()
	c := newConnection(sock, fd, remote, connID, true, cfg)
	c.start()

	if err := c.sendHandshake(connID); err != nil {
		c.Close()
		return nil, err
	}

	// Wait for the handshake ack, resending at a fixed interval (no
	// backoff) until it arrives or the overall handshake timeout elapses.
	deadline := time.NewTimer(cfg.handshakeTimout)
	defer deadline.Stop()
	retry := time.NewTicker(defaultHandshakeRetryIvl)
	defer retry.Stop()

	for {
		select {
		case <-c.establishedCh:
			return c, nil
		case <-ctx.Done():
			c.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			c.Close()
			return nil, ErrHandshakeTimeout
		case <-retry.C:
			if err := c.sendHandshake(connID); err != nil {
				c.Close()
				return nil, err
			}
		}
	}
}

func (c *Connection) sendHandshake(connID uint64) error {
	payload := handshakePayload(connID)
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeFrame(buf, frameHandshake, 0, 0, payload)
	if err := c.commitSendSlot(idx, n); err != nil {
		return err
	}
	return c.flushSend()
}

func generateConnID() uint64 {
	var b [8]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}

func parseAddr(addr string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	host = h
	pn, err := net.LookupPort("udp", p)
	if err != nil {
		return
	}
	port = pn
	return
}

// Returns negative RTT if no ping-pong exchange has completed yet.
func (c *Connection) GetRtt() int64 {
	return c.rttMs.Load()
}

// updateRTO folds a new RTT sample into the SRTT/RTTVAR estimate (RFC 6298,
// the same Jacobson/Karels algorithm TCP uses) and republishes the resulting
// RTO. Samples come from two sources: DATA frames that were ACKed without
// ever being retransmitted (handleACK — excluding retransmitted frames
// follows Karn's algorithm, since an ACK for a multiply-sent frame can't be
// attributed to a specific transmission), and the keep-alive PING/PONG
// round trip as a fallback when no data is flowing. Caller holds recvMu.
func (c *Connection) updateRTO(sample time.Duration) {
	if sample <= 0 {
		return
	}
	if c.srtt == 0 {
		c.srtt = sample
		c.rttvar = sample / 2
	} else {
		diff := c.srtt - sample
		if diff < 0 {
			diff = -diff
		}
		c.rttvar += (diff - c.rttvar) / 4
		c.srtt += (sample - c.srtt) / 8
	}
	rto := c.srtt + 4*c.rttvar
	// cfg.retransmitTmout doubles as a floor: it's the best guess available
	// before any real samples exist, and afterwards it protects against a
	// burst of atypically fast samples driving the RTO low enough to cause
	// spurious retransmissions.
	if min := c.cfg.retransmitTmout; rto < min {
		rto = min
	}
	c.rtoNs.Store(int64(rto))
}

// currentRTO returns the connection's current adaptive retransmit timeout.
// Safe to call from any goroutine without recvMu.
func (c *Connection) currentRTO() time.Duration {
	return time.Duration(c.rtoNs.Load())
}

func (c *Connection) SendPing(streamId uint32, seq uint32) error {
	c.sendPingMs.Store(time.Now().UnixMilli())
	err := c.sendControlFrame(framePing, streamId, seq)
	if err != nil {
		c.sendPingMs.Store(0)
	}
	return err
}

// sendRaw sends a raw datagram directly via the socket fd. Used during
// listener handshake before a full connection is established.
func sendRawDatagram(fd int, to syscall.Sockaddr, buf []byte, n int) error {
	return syscall.Sendto(fd, buf[:n], 0, to)
}

// recvRawDatagram receives a single datagram. Used during listener setup.
func recvRawDatagram(fd int, buf []byte) (n int, from syscall.Sockaddr, err error) {
	n, from, err = syscall.Recvfrom(fd, buf, 0)
	return
}

// sendToAddr sends a datagram to a specific address using sendmsg.
// Used by listener for unconnected socket communication.
func sendToAddr(fd int, addr *unet.Address, data []byte, n int) error {
	sa := addr.AsSockaddr()
	return syscall.Sendto(fd, data[:n], 0, sa)
}

// Ensure Connection.sendEP/recvEP iov pointers stay on heap.
// This helps prevent GC from moving the pointed-to buffers.
var _ = unsafe.Sizeof(unet.UdpEndpoint{})
