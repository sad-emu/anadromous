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
	streamMu          sync.RWMutex
	streams           map[uint32]*Stream
	nextStream        uint32 // next stream ID to allocate
	acceptCh          chan *Stream
	sReadReorderBuff  map[uint32]map[uint32][]byte // out-of-order data waiting for missing sequence numbers
	sReadReorderCount map[uint32]int               // total buffered out-of-order bytes
	sReadReorderMax   int                          // max count of out-of-order packets per stream
	sReadIdsToAck     map[uint32][]uint32          // List of packet IDs to ACK for each stream

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

	// role: true if this side initiated the connection (client)
	isClient bool
}

// newConnection creates a Connection around an already-bound unet.Socket.
// The socket must be a UDP socket with NearAddr and FarAddr set.
func newConnection(sock *unet.Socket, fd int, remote unet.Address, connID uint64, isClient bool, cfg config) *Connection {
	c := &Connection{
		cfg:              cfg,
		connID:           connID,
		sock:             sock,
		fd:               fd,
		remoteAddr:       remote,
		streams:          make(map[uint32]*Stream, 64),
		acceptCh:         make(chan *Stream, cfg.maxStreams),
		closeCh:          make(chan struct{}),
		isClient:         isClient,
		sReadReorderBuff: make(map[uint32]map[uint32][]byte),
		sReadReorderMax:  10000,
		retransmit:       newRetransmitQueue(),
		establishedCh:    make(chan struct{}),
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

	// Set up batched receive endpoint.
	c.sReadReorderCount = make(map[uint32]int)
	c.sReadIdsToAck = make(map[uint32][]uint32)
	c.recvBufs = make([][]byte, cfg.batchSize)
	recvIdx := 0
	c.recvEP.SetupVectors(cfg.batchSize, 1, func(iov []syscall.Iovec) {
		b := make([]byte, maxDatagramSize)
		c.recvBufs[recvIdx] = b
		iov[0].Base = &b[0]
		iov[0].Len = uint64(maxDatagramSize)
		recvIdx++
	}, nil) // connected socket, no name needed

	// Set up batched send endpoint.
	c.sendBufs = make([][]byte, cfg.batchSize)
	c.sendLens = make([]int, cfg.batchSize)
	sendIdx := 0
	c.sendEP.SetupVectors(cfg.batchSize, 1, func(iov []syscall.Iovec) {
		b := make([]byte, maxDatagramSize)
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

// OpenStream creates a new outbound stream.
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

	// Tell the peer about the new stream.
	if err := c.sendControlFrame(frameStreamOpen, id, 0); err != nil {
		c.removeStream(id)
		return nil, err
	}
	return s, nil
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
	c.retransmit.add(streamID, seq, payload)
	return nil
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
		c.sendEP.Iov[i].Len = uint64(maxDatagramSize)
		c.sendEP.Hdrs[i].NTransferred = 0
	}

	if errno != 0 {
		return errno
	}
	return nil
}

// --- background loops ---

// retransmitLoop periodically resends any DATA frame that has been
// outstanding for longer than the configured (fixed, non-backing-off)
// retransmit timeout. Streams that exceed the retry budget are failed.
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

		resend, exceeded := c.retransmit.due(c.cfg.retransmitTmout, c.cfg.retransmitRetries)

		for _, e := range resend {
			buf, idx, err := c.acquireSendSlot()
			if err != nil {
				break // connection is going away
			}
			n := encodeFrame(buf, frameData, e.streamID, e.seq, e.data)
			c.commitSendSlot(idx, n)
		}
		if len(resend) > 0 {
			c.flushSend()
		}

		for _, e := range exceeded {
			c.streamMu.RLock()
			s, ok := c.streams[e.streamID]
			c.streamMu.RUnlock()
			if ok {
				s.deliverError(ErrRetransmitExceeded)
				c.removeStream(e.streamID)
			}
		}
	}
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
			c.recvEP.Iov[i].Len = uint64(maxDatagramSize)
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

	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	switch f.ftype {
	case frameData:
		c.handleData(f)
	case frameStreamOpen:
		c.handleStreamOpen(f)
	case frameStreamClose:
		c.handleStreamClose(f)
	case frameStreamFIN:
		c.handleStreamFIN(f)
	case framePing:
		c.sendControlFrame(framePong, f.streamID, f.seq)
	case framePong:
		atomic.StoreInt32(&c.missedPongs, 0)
		if sentAt := c.sendPingMs.Load(); sentAt > 0 {
			c.rttMs.Store(time.Now().UnixMilli() - sentAt)
			c.sendPingMs.Store(0)
		}
	case frameGoAway:
		go c.Close()
	case frameACK:
		c.handleACK(f)
	case frameHandshake:
		c.handleHandshake(f)
	default:
		// unknown frame type, ignore
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

	for streamID, seqs := range pending {
		for len(seqs) > 0 {
			n := len(seqs)
			if n > maxAckSeqsPerFrame {
				n = maxAckSeqsPerFrame
			}
			c.sendACKFrame(streamID, seqs[:n])
			seqs = seqs[n:]
		}
	}
}

func (c *Connection) handleData(f frame) {
	c.streamMu.RLock()
	s, ok := c.streams[f.streamID]
	c.streamMu.RUnlock()

	if !ok {
		return // stream not found, discard
	}

	// ACK every valid frame for a known stream, even duplicates: if our
	// previous ACK for this seq was lost, the sender will have retransmitted
	// it, and it must be re-ACKed or the sender retransmits forever.
	if c.sReadIdsToAck[f.streamID] == nil {
		c.sReadIdsToAck[f.streamID] = make([]uint32, 0)
	}
	c.sReadIdsToAck[f.streamID] = append(c.sReadIdsToAck[f.streamID], f.seq)

	if s.readSeq > f.seq {
		return // already delivered, discard payload but keep the ACK above
	}

	if c.sReadReorderCount[f.streamID] > c.sReadReorderMax {
		// close the stream for excessive out-of-order buffering
		c.purgeStreamState(f.streamID)
		s.deliverClose()
		c.removeStream(f.streamID)
		return
	}

	if s.readSeq < f.seq {
		// Out-of-order frame, buffer for later
		if c.sReadReorderBuff[f.streamID] == nil {
			c.sReadReorderBuff[f.streamID] = make(map[uint32][]byte)
		}
		c.sReadReorderBuff[f.streamID][f.seq] = f.payload
		c.sReadReorderCount[f.streamID] = c.sReadReorderCount[f.streamID] + 1
		return
	}

	s.readSeq++
	s.deliverData(f.payload)

	// Initial implementation for out of order recovery
	if c.sReadReorderBuff[f.streamID] != nil {
		totalReorders := c.sReadReorderCount[f.streamID]
		for i := 0; i < totalReorders; i++ {
			if c.sReadReorderBuff[f.streamID][s.readSeq] != nil {
				s.deliverData(c.sReadReorderBuff[f.streamID][s.readSeq])
				delete(c.sReadReorderBuff[f.streamID], s.readSeq)
				s.readSeq++
				c.sReadReorderCount[f.streamID] = c.sReadReorderCount[f.streamID] - 1
			} else {
				break
			}
		}
	}
}

func (c *Connection) handleStreamOpen(f frame) {
	c.streamMu.Lock()
	if _, exists := c.streams[f.streamID]; exists {
		c.streamMu.Unlock()
		return // duplicate open, ignore
	}
	if len(c.streams) >= c.cfg.maxStreams {
		c.streamMu.Unlock()
		return // at capacity, ignore
	}
	s := newStream(f.streamID, c, c.cfg.streamBufSize)
	c.streams[f.streamID] = s
	c.streamMu.Unlock()

	// Non-blocking send to accept channel.
	select {
	case c.acceptCh <- s:
	default:
		// Accept channel full — stream will be available via map but not accepted.
	}
}

func (c *Connection) handleStreamClose(f frame) {
	c.streamMu.Lock()
	s, ok := c.streams[f.streamID]
	if ok {
		delete(c.streams, f.streamID)
	}
	c.streamMu.Unlock()

	c.purgeStreamState(f.streamID)

	if ok {
		s.deliverClose()
	}
}

func (c *Connection) handleStreamFIN(f frame) {
	c.streamMu.RLock()
	s, ok := c.streams[f.streamID]
	c.streamMu.RUnlock()

	if ok {
		s.deliverFIN()
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
	c.retransmit.ackMany(f.streamID, seqs)
}

// purgeStreamState removes all connection-level bookkeeping for a stream
// (reorder buffer, pending acks, retransmit queue entries). Caller must
// already hold recvMu (i.e. be called from within handleDatagram's dispatch).
func (c *Connection) purgeStreamState(streamID uint32) {
	delete(c.sReadReorderBuff, streamID)
	delete(c.sReadReorderCount, streamID)
	delete(c.sReadIdsToAck, streamID)
	c.retransmit.purgeStream(streamID)
}

func (c *Connection) removeStream(id uint32) {
	c.streamMu.Lock()
	delete(c.streams, id)
	c.streamMu.Unlock()
	c.retransmit.purgeStream(id)
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
		SetOptSndBuf(cfg.sendBufSize)

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
