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
	sendN    int      // number of pending datagrams in current batch

	// stream management
	streamMu      sync.RWMutex
	streams       map[uint32]*Stream
	nextStream    uint32 // next stream ID to allocate
	acceptCh      chan *Stream
	sReadIdsToAck map[uint32][]uint32 // packet IDs to ACK per stream (recvMu)

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

	// statResends counts frames retransmitted over the connection lifetime.
	statResends atomic.Int64

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
		retransmit:    newRetransmitQueue(),
		establishedCh: make(chan struct{}),
		deadStreams:   make(map[uint32]time.Time),
		streamFreedCh: make(chan struct{}, 1),
	}
	c.rttMs.Store(-1)

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

	// Set up batched receive endpoint.
	c.sReadIdsToAck = make(map[uint32][]uint32)
	c.recvBufs = make([][]byte, cfg.batchSize)
	recvIdx := 0
	c.recvEP.SetupVectors(cfg.batchSize, 1, func(iov []syscall.Iovec) {
		b := make([]byte, maxDatagram)
		c.recvBufs[recvIdx] = b
		iov[0].Base = &b[0]
		iov[0].Len = uint64(maxDatagram)
		recvIdx++
	}, nil) // connected socket, no name needed

	// Set up batched send endpoint.
	c.sendBufs = make([][]byte, cfg.batchSize)
	c.sendLens = make([]int, cfg.batchSize)
	sendIdx := 0
	c.sendEP.SetupVectors(cfg.batchSize, 1, func(iov []syscall.Iovec) {
		b := make([]byte, maxDatagram)
		c.sendBufs[sendIdx] = b
		iov[0].Base = &b[0]
		iov[0].Len = 0 // set to actual frame size on each send
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

// sendDataFrame queues a DATA frame and records it for retransmission until
// acknowledged. It does not flush; callers that want the frame on the wire
// immediately (or after queuing several) must call flushSend().
func (c *Connection) sendDataFrame(streamID, seq uint32, payload []byte) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeFrame(buf, frameData, streamID, seq, payload)
	if err := c.commitSendSlot(idx, n); err != nil {
		return err
	}
	c.retransmit.add(frameData, streamID, seq, payload)
	return nil
}

// sendReliableFrame sends a frame immediately and retransmits it until the
// peer acknowledges its (streamID, seq). Used for FIN and RESET frames,
// which occupy the sequence position after the stream's last data frame.
func (c *Connection) sendReliableFrame(ftype uint8, streamID, seq uint32, payload []byte) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeFrame(buf, ftype, streamID, seq, payload)
	if err := c.commitSendSlot(idx, n); err != nil {
		return err
	}
	c.retransmit.add(ftype, streamID, seq, payload)
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

// sendACKFrame sends an ACK frame acknowledging receipt of a set of data
// frames immediately. len(seqs) must not exceed maxAckSeqsPerFrame.
func (c *Connection) sendACKFrame(streamID uint32, seqs []uint32) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeAckFrame(buf, streamID, seqs)
	if err := c.commitSendSlot(idx, n); err != nil {
		return err
	}
	return c.flushSend()
}

// acquireSendSlot reserves a slot in the send batch. If the batch is full,
// it flushes first.
func (c *Connection) acquireSendSlot() (buf []byte, idx int, err error) {
	c.sendMu.Lock()
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

// commitSendSlot finishes writing to a send slot, flushing only if the
// batch is now full. Caller holds sendMu (acquired by acquireSendSlot) and
// this releases it.
func (c *Connection) commitSendSlot(idx int, n int) error {
	// Update the iov length for this message.
	c.sendEP.Iov[idx].Len = uint64(n)
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

// flushSend pushes any queued-but-unflushed datagrams to the wire in a
// single sendmmsg call.
func (c *Connection) flushSend() error {
	c.sendMu.Lock()
	err := c.flushSendLocked()
	c.sendMu.Unlock()
	return err
}

// flushSendLocked sends all queued datagrams via sendmmsg. Caller holds sendMu.
func (c *Connection) flushSendLocked() error {
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

	// Reset iov lens for next batch.
	for i := 0; i < n; i++ {
		c.sendEP.Iov[i].Len = uint64(c.cfg.maxDatagram())
		c.sendEP.Hdrs[i].NTransferred = 0
	}

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

		resend := c.retransmit.due(c.cfg.retransmitTmout)

		for _, e := range resend {
			buf, idx, err := c.acquireSendSlot()
			if err != nil {
				break // connection is going away
			}
			n := encodeFrame(buf, e.ftype, e.streamID, e.seq, e.data)
			c.commitSendSlot(idx, n)
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
			c.recvEP.Iov[i].Len = uint64(c.cfg.maxDatagram())
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

		for i := 0; i < messages; i++ {
			nbytes := int(c.recvEP.Hdrs[i].NTransferred)
			if nbytes < frameHeaderSize {
				continue // too small, discard
			}
			c.handleDatagram(c.recvBufs[i][:nbytes])
		}

		c.flushPendingAcks()
	}
}

// handleDatagram processes a single received datagram. Safe to call from
// any goroutine (see recvMu).
func (c *Connection) handleDatagram(buf []byte) {
	f, err := decodeFrame(buf)
	if err != nil {
		return // discard malformed frames
	}

	// Any successfully decoded frame from the peer is proof of life: reset
	// the idle/keepalive counter here rather than only on framePong. Ping is
	// a tiny, unreliable, single-packet control frame like any other, so
	// under loss it (or its reply) can be dropped several times in a row
	// even while the connection is actively exchanging bulk DATA/ACK traffic
	// — that's not an idle peer, and treating it as one contradicts
	// WithIdleTimeout's documented "equivalent to quic-go's MaxIdleTimeout"
	// behavior, where the idle timer resets on any received packet.
	atomic.StoreInt32(&c.missedPongs, 0)

	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	switch f.ftype {
	case frameData:
		c.handleData(f)
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
			c.rttMs.Store(time.Now().UnixMilli() - sentAt)
			c.sendPingMs.Store(0)
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

// flushPendingAcks sends any accumulated ACKs, chunked to fit the wire
// format, then clears the pending list. Called after each receive batch.
func (c *Connection) flushPendingAcks() {
	c.recvMu.Lock()
	if len(c.sReadIdsToAck) == 0 {
		c.recvMu.Unlock()
		return
	}
	pending := c.sReadIdsToAck
	c.sReadIdsToAck = make(map[uint32][]uint32)
	c.recvMu.Unlock()

	maxAcks := maxAckSeqsPerFrame(c.cfg.maxPayload)
	for streamID, seqs := range pending {
		for len(seqs) > 0 {
			n := len(seqs)
			if n > maxAcks {
				n = maxAcks
			}
			c.sendACKFrame(streamID, seqs[:n])
			seqs = seqs[n:]
		}
	}
}

// queueAck records a (stream, seq) for the next ACK flush. Caller holds recvMu.
func (c *Connection) queueAck(streamID, seq uint32) {
	c.sReadIdsToAck[streamID] = append(c.sReadIdsToAck[streamID], seq)
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
	if s.deliver(f.seq, f.payload, false) {
		c.queueAck(f.streamID, f.seq)
	}
}

func (c *Connection) handleStreamFIN(f frame) {
	s, ackDiscard := c.streamForFrame(f.streamID)
	if s == nil {
		if ackDiscard {
			c.queueAck(f.streamID, f.seq)
		}
		return
	}
	if s.deliver(f.seq, nil, true) {
		c.queueAck(f.streamID, f.seq)
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
	seqs, err := decodeAckFrame(f)
	if err != nil {
		return // discard malformed ACK
	}
	freed := c.retransmit.ackMany(f.streamID, seqs)
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

// handleNack immediately resends the single (stream, seq) frame the peer is
// missing, bypassing the fixed-interval retransmit timer. This is the
// fast-retransmit path: the peer only sends a NACK once it has direct
// evidence of the gap (a later frame already arrived), so recovery happens
// in about one RTT instead of waiting out the timer.
func (c *Connection) handleNack(f frame) {
	e, ok := c.retransmit.getForResend(f.streamID, f.seq)
	if !ok {
		return // already acknowledged, or a stale/unknown NACK
	}
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return
	}
	n := encodeFrame(buf, e.ftype, e.streamID, e.seq, e.data)
	if c.commitSendSlot(idx, n) == nil {
		c.statResends.Add(1)
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
