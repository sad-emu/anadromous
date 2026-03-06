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

	// peer address
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
	streamMu   sync.RWMutex
	streams    map[uint32]*Stream
	nextStream uint32 // next stream ID to allocate
	acceptCh   chan *Stream

	// connection lifecycle
	closed   atomic.Bool
	closeCh  chan struct{}
	closeErr error
	doneWg   sync.WaitGroup

	// role: true if this side initiated the connection (client)
	isClient bool
}

// newConnection creates a Connection around an already-bound unet.Socket.
// The socket must be a UDP socket with NearAddr and FarAddr set.
func newConnection(sock *unet.Socket, fd int, remote unet.Address, connID uint64, isClient bool, cfg config) *Connection {
	c := &Connection{
		cfg:        cfg,
		connID:     connID,
		sock:       sock,
		fd:         fd,
		remoteAddr: remote,
		streams:    make(map[uint32]*Stream, 64),
		acceptCh:   make(chan *Stream, cfg.maxStreams),
		closeCh:    make(chan struct{}),
		isClient:   isClient,
	}

	// Client-initiated streams use odd IDs (1,3,5,...).
	// Server-initiated streams use even IDs (2,4,6,...).
	if isClient {
		c.nextStream = 1
	} else {
		c.nextStream = 2
	}

	c.sock.GetNearAddress(&c.localAddr)

	// Set up batched receive endpoint.
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

// start begins the read loop.
func (c *Connection) start() {
	c.doneWg.Add(1)
	go c.readLoop()
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

	// Send GOAWAY to peer (best-effort).
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

// sendDataFrame queues and sends a DATA frame.
func (c *Connection) sendDataFrame(streamID, seq uint32, payload []byte) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeFrame(buf, frameData, streamID, seq, payload)
	return c.commitSendSlot(idx, n)
}

// sendControlFrame sends a control frame (no payload).
func (c *Connection) sendControlFrame(ftype uint8, streamID, seq uint32) error {
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	encodeHeader(buf, ftype, streamID, seq, 0)
	return c.commitSendSlot(idx, frameHeaderSize)
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

// commitSendSlot finishes writing to a send slot and flushes if batch is full.
func (c *Connection) commitSendSlot(idx int, n int) error {
	// Update the iov length for this message.
	c.sendEP.Iov[idx].Len = uint64(n)
	c.sendEP.Hdrs[idx].NTransferred = 0
	c.sendLens[idx] = n
	c.sendN = idx + 1

	if c.sendN >= c.cfg.batchSize {
		err := c.flushSendLocked()
		c.sendMu.Unlock()
		return err
	}
	// For low-latency, flush immediately for now. A future optimization
	// could batch sends with a short timer.
	err := c.flushSendLocked()
	c.sendMu.Unlock()
	return err
}

// flushSendLocked sends all queued datagrams via sendmmsg. Caller holds sendMu.
func (c *Connection) flushSendLocked() error {
	if c.sendN == 0 {
		return nil
	}
	if c.closed.Load() {
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
	}
}

// handleDatagram processes a single received datagram.
func (c *Connection) handleDatagram(buf []byte) {
	f, err := decodeFrame(buf)
	if err != nil {
		return // discard malformed frames
	}

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
		c.sendControlFrame(framePong, 0, f.seq)
	case framePong:
		// could track RTT here
	case frameGoAway:
		go c.Close()
	case frameACK:
		c.handleACK(f)
	default:
		// unknown frame type, ignore
	}
}

func (c *Connection) handleData(f frame) {
	c.streamMu.RLock()
	s, ok := c.streams[f.streamID]
	c.streamMu.RUnlock()

	if !ok {
		return // stream not found, discard
	}

	// TODO: reorder by f.seq if needed (retransmission stub)
	s.deliverData(f.payload)
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

func (c *Connection) handleACK(f frame) {
	// TODO: retransmission stub — remove acked frames from retransmit queue
}

func (c *Connection) removeStream(id uint32) {
	c.streamMu.Lock()
	delete(c.streams, id)
	c.streamMu.Unlock()
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

	// Send handshake.
	if err := c.sendHandshake(connID); err != nil {
		c.Close()
		return nil, err
	}

	// Wait for handshake response.
	// The read loop will receive the handshake ACK. For now, we consider
	// the connection established once the handshake is sent.
	// TODO: wait for handshake ACK with timeout

	return c, nil
}

func (c *Connection) sendHandshake(connID uint64) error {
	payload := handshakePayload(connID)
	buf, idx, err := c.acquireSendSlot()
	if err != nil {
		return err
	}
	n := encodeFrame(buf, frameHandshake, 0, 0, payload)
	return c.commitSendSlot(idx, n)
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
