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

	// receive scratch buffers — one per recvmmsg slot
	recvBufs [][]byte

	// send queue: frames are queued and flushed in batches, either
	// synchronously via sendmmsg or pipelined via io_uring (see uring.go).
	// The io_uring path double-buffers: two complete slot sets (buffers,
	// iovecs, msghdrs) alternate, so batch N can be in flight in the kernel
	// while batch N+1 is being built in the other set. Without io_uring only
	// set 0 exists and never flips.
	sendMu      sync.Mutex
	sendEPs     [2]unet.UdpEndpoint
	sendBufSets [2][][]byte
	sendEP      *unet.UdpEndpoint // active set's endpoint (== &sendEPs[uringSet])
	sendBufs    [][]byte          // active set's datagram buffers
	sendLens    []int             // actual length written into each sendBuf
	sendN       int               // number of pending messages in current batch
	iovsPer     int               // iovecs per message slot (2*gsoMaxFrames, or 2 without GSO)
	// Exact carrier-accounted cost accumulated while frames enter this batch.
	// GSO frames increment datagrams individually even though they share one
	// sendmmsg message. The completed aggregate is granted once at flush.
	sendWireBytes int64
	sendUDPBytes  int64
	sendDatagrams int
	sendPaceClass paceClass

	// io_uring send state (sendMu). uring == nil means the sendmmsg path.
	uring     *sendRing
	uringSet  int // active slot set, doubling as the CQE generation tag
	uringMark int // retransmit pendingFree watermark taken at the last flush

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
	resendScratch []retransmitRequest // reused NACK-to-worker batch (recvMu)

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

	// resendQueue is an unbounded, de-duplicated work queue of retransmit
	// keys. NACK handling only appends keys while recvMu is held; the
	// retransmitLoop revalidates them and builds recovery-priority batches
	// without holding receive/stream locks. The shared pacer grants each
	// completed batch when it is flushed.
	resendMu    sync.Mutex
	resendQueue []retransmitRequest
	resendHead  int
	resendWake  chan struct{}

	// inFlightCapLan/Wan are the two per-stream caps on sent-but-unACKed
	// bytes that inFlightCap switches between by measured RTT (see that
	// method for the physics). Fixed at construction: an explicit
	// WithMaxBytesInFlight sets both; otherwise they're derived from the
	// socket's actually-granted SO_RCVBUF — see newConnection.
	inFlightCapLan int64
	inFlightCapWan int64

	// wirePacer may be shared by every Connection on one bridge. All frame
	// paths are accounted as they enter a batch and the pacer is visited only
	// once when that batch is handed to the kernel.
	wirePacer *WirePacer

	// onClose, if set, is invoked once when the connection closes (used by
	// Listener to remove the connection from its tracking map).
	onClose func()

	// established is closed once the handshake completes (client side).
	establishedCh   chan struct{}
	establishedOnce sync.Once

	// connection lifecycle
	closed    atomic.Bool
	closeCh   chan struct{}
	closeDone chan struct{} // closed after fd/goroutine teardown is complete
	closeErr  error
	doneWg    sync.WaitGroup

	// Statuses (accessed from the public API concurrently with readLoop, so
	// these must be atomic rather than plain fields).
	sendPingMs  atomic.Int64
	rttMs       atomic.Int64
	missedPongs int32

	// RTT/RTO estimation (RFC 6298, Jacobson/Karels): srtt/rttvar are only
	// touched by updateRTO, called from handleACK and the PONG handler,
	// both of which already run under recvMu. rtoNs and rttvarNs are the
	// published, lock-free views that retransmitLoop and fast-retransmit
	// read from any goroutine — see currentRTO and reorderGrace.
	srtt     time.Duration
	rttvar   time.Duration
	rtoNs    atomic.Int64
	rttvarNs atomic.Int64
	srttNs   atomic.Int64

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
		closeDone:     make(chan struct{}),
		isClient:      isClient,
		retransmit:    newRetransmitQueue(cfg.maxPayload),
		resendWake:    make(chan struct{}, 1),
		establishedCh: make(chan struct{}),
		deadStreams:   make(map[uint32]time.Time),
		streamFreedCh: make(chan struct{}, 1),
		wirePacer:     cfg.wirePacer,
		sendPaceClass: paceBulk,
	}
	c.rttMs.Store(-1)
	c.rtoNs.Store(int64(cfg.retransmitTmout))

	// Resolve the in-flight caps; inFlightCap picks between them by
	// measured RTT, because RTT decides where the window physically lives:
	//
	//   - LAN (short RTT): the wire stores almost nothing, so everything
	//     outstanding piles into the receiver's SOCKET buffer, and the cap
	//     must fit its usable payload capacity: granted/2, the un-doubled
	//     request (the kernel doubles SO_RCVBUF requests precisely because
	//     it charges arriving data at skb truesize — payload plus
	//     bookkeeping overhead). Without this cap the sender's only brake
	//     is flow control, whose window is far larger than the socket
	//     buffer, and a faster-than-receiver sender collapses into the
	//     drop→retransmit spiral documented on WithMaxBytesInFlight.
	//   - WAN (real RTT), PACED: no socket-derived cap at all. The window
	//     is stored on the wire, and the receiver's real capacity is its
	//     flow-controlled stream ring (WithStreamBufferSize), which already
	//     bounds bytesInFlight via Stream.inFlightCap's stream-buffer
	//     clamp. This protocol never backs off by design — links are
	//     provisioned, and the goal is to keep the pipe full through loss —
	//     so tying the WAN window to the (comparatively tiny) socket buffer
	//     just capped throughput at socketBuffer/RTT for no protective
	//     benefit: on a real path the socket buffer only has to absorb
	//     readLoop scheduling gaps, not the whole window.
	//   - WAN, UNPACED: granted − granted/4, the bounded window. The
	//     uncapped window is only safe when the pacer keeps the sender's
	//     bursts at the pipe's known rate; an unpaced sender bursts at line
	//     rate, and if that overruns the bottleneck, the self-inflicted
	//     drop storm plays out across the whole (huge) window — Write then
	//     stalls for seconds mid-recovery, which applications above (e.g. a
	//     TCP bridge) experience as their own I/O timeouts. The bounded
	//     window keeps stall depth at ~1 RTT instead.
	//
	// An explicit WithMaxBytesInFlight replaces both.
	c.inFlightCapLan = int64(cfg.effectiveMaxInFlight())
	c.inFlightCapWan = c.inFlightCapLan
	if cfg.maxInFlight == 0 {
		if granted, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF); err == nil && granted > 0 {
			c.inFlightCapLan = int64(granted / 2)
			c.inFlightCapWan = int64(granted - granted/4)
		}
		if cfg.pacingEnabled() {
			c.inFlightCapWan = int64(1) << 62 // stream-buffer clamp governs
		}
	}

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

	// Set up batched send endpoint(s). Each message slot has iovsPer iovecs
	// used in (header, payload) pairs: iov[0] is the slot's own buffer,
	// holding either a fully-encoded frame (header and payload combined,
	// for the low-traffic control frame types) or one or more 13-byte
	// headers (for DATA/FIN/RESET, whose payloads live in the retransmit
	// arena); each odd iovec points directly at an arena payload so bulk
	// data is never copied into a send buffer at all (see
	// commitSendSlotZeroCopy and queueFrameGSOLocked).
	c.sendLens = make([]int, cfg.batchSize)
	setupSendEP := func(set int) {
		bufs := make([][]byte, cfg.batchSize)
		c.sendBufSets[set] = bufs
		idx := 0
		c.sendEPs[set].SetupVectors(cfg.batchSize, c.iovsPer, func(iov []syscall.Iovec) {
			b := make([]byte, maxDatagram)
			bufs[idx] = b
			iov[0].Base = &b[0]
			iov[0].Len = 0 // set to actual frame size on each send
			for i := 1; i < len(iov); i++ {
				iov[i].Base = nil
				iov[i].Len = 0
			}
			idx++
		}, nil) // connected socket, no name needed
	}
	setupSendEP(0)
	c.sendEP = &c.sendEPs[0]
	c.sendBufs = c.sendBufSets[0]

	// Try to set up the pipelined io_uring send path; the ring must hold two
	// full batches (both slot sets in flight at the worst moment). Falls back
	// to sendmmsg when unavailable.
	if cfg.enableIOUring {
		if c.uring = newSendRing(2 * cfg.batchSize); c.uring != nil {
			setupSendEP(1)
		}
	}

	return c
}

// start begins the read loop and the background retransmit/keepalive loops.
func (c *Connection) start() {
	// Register every worker before any of them can observe a queued GOAWAY and
	// start Close. Incremental Add calls would let the first worker drop the
	// count to zero while Close is in Wait, followed by a late Add/use of the
	// already-torn-down socket.
	workers := 2
	if c.cfg.keepAlive > 0 {
		workers++
	}
	c.doneWg.Add(workers)
	go c.readLoop()
	go c.retransmitLoop()
	if c.cfg.keepAlive > 0 {
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
	// Close may have won after the optimistic check above while OpenStream was
	// waiting for streamMu. Recheck at the map-mutation linearization point.
	if c.closed.Load() || c.streams == nil {
		c.streamMu.Unlock()
		return nil, ErrClosed
	}
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
		// If closeCh and an already-buffered stream become ready together,
		// select may choose either arm. Do not publish a stream after teardown
		// has started.
		if c.closed.Load() {
			return nil, ErrClosed
		}
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
		// Close is synchronous even when another goroutine won the transition.
		// Listener.Close relies on this when it encounters a connection already
		// part-way through an individual Close.
		if c.closeDone != nil {
			<-c.closeDone
		}
		return nil
	}
	if c.closeDone != nil {
		defer close(c.closeDone)
	}

	// Cancel any bulk/recovery batch currently waiting in the shared pacer
	// before trying to acquire sendMu below. Without this ordering, Close can
	// sit behind an arbitrarily long pacing wait and never reach the channel
	// close that would release it. GOAWAY remains best-effort after cancellation;
	// when the control reserve cannot grant it immediately, socket shutdown is
	// what informs the peer.
	close(c.closeCh)

	// Send GOAWAY to peer (best-effort). The socket is still open at this
	// point (Shutdown happens below), so this reaches flushSendLocked fine.
	c.sendControlFrame(frameGoAway, 0, 0)

	// On the io_uring path that send is asynchronous — drain it before the
	// Shutdown below, or the shutdown races the io-wq worker and the GOAWAY
	// (and any other still-in-flight frames) dies with the socket, leaving
	// the peer to find out via keepalive timeout instead.
	c.sendMu.Lock()
	if c.uring != nil {
		c.uring.waitGen(0)
		c.uring.waitGen(1)
	}
	c.sendMu.Unlock()

	// Exclude both the dedicated read loop and the Listener's fallback path,
	// then shut down the connected socket while ingress is excluded. A reader
	// that was already blocked in RecvMMsg wakes and must reacquire recvMu,
	// where it observes closed before dispatching a late DATA/FIN.
	c.recvMu.Lock()
	c.sock.Shutdown()

	// Snapshot and clear stream state only after ingress has been fenced. Do
	// not call into Stream while holding streamMu: application callbacks may
	// concurrently be unwinding their stream state.
	var streams []*Stream
	c.streamMu.Lock()
	for _, s := range c.streams {
		streams = append(streams, s)
	}
	c.streams = nil
	c.streamMu.Unlock()
	c.recvMu.Unlock()

	for _, s := range streams {
		s.deliverClose()
	}

	// Wait for read/retransmit/keepalive loops to leave before releasing socket
	// and io_uring storage. recvMu is deliberately not held across this wait.
	c.doneWg.Wait()

	// Drain and release the io_uring before the socket fd goes away — its
	// in-flight sends reference the fd and the slot buffers.
	c.sendMu.Lock()
	if c.uring != nil {
		c.uring.close()
		c.uring = nil
	}
	c.sendMu.Unlock()

	// Close the socket fd.
	c.sock.Close()

	// Keep listener-owned connections discoverable until teardown really is
	// complete. A concurrent Listener.Close can then snapshot this connection
	// and wait through the synchronous losing-Close path above.
	if c.onClose != nil {
		c.onClose()
	}
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

// accountDatagramLocked adds one final encoded UDP datagram to the current
// batch's carrier-visible cost. It is intentionally arithmetic-only: the
// shared pacer's clock, mutex and any wait are visited once by flushSendLocked
// rather than once per frame. Caller holds sendMu.
func (c *Connection) accountDatagramLocked(udpBytes int, class paceClass) {
	if c.wirePacer == nil {
		return
	}
	if c.sendDatagrams == 0 {
		c.sendPaceClass = class
	} else if class > c.sendPaceClass {
		// Mixed classes should have been split before accounting. If a future
		// caller misses that boundary, fall back to the least-privileged class
		// so bulk bytes can never inherit a critical/recovery exemption.
		c.sendPaceClass = class
	}
	c.sendUDPBytes += int64(udpBytes)
	c.sendWireBytes += c.wirePacer.accounting.Cost(udpBytes)
	c.sendDatagrams++
}

// splitPacingClassLocked keeps paced batches homogeneous. In particular, an
// ACK or window update arriving while DATA is pending must not turn all of the
// DATA into a critical batch that bypasses waiting. Caller holds sendMu and
// must have finalized any open GSO pack first.
func (c *Connection) splitPacingClassLocked(class paceClass) error {
	if c.wirePacer != nil && c.sendDatagrams > 0 && c.sendPaceClass != class {
		return c.flushSendLocked()
	}
	return nil
}

// pacingBatchFullLocked reports whether the accumulated wire charge has
// reached the configured burst quantum. Auto-flushing here bounds actual
// kernel emission; merely acquiring tokens per frame and releasing a much
// larger sendmmsg/GSO batch would still create a line-rate burst.
func (c *Connection) pacingBatchFullLocked() bool {
	return c.wirePacer != nil && c.sendWireBytes >= c.wirePacer.burstBytes
}

func (c *Connection) resetBatchAccountingLocked() {
	c.sendWireBytes = 0
	c.sendUDPBytes = 0
	c.sendDatagrams = 0
	c.sendPaceClass = paceBulk
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
	buf, idx, err := c.acquireSendSlot(paceBulk)
	if err != nil {
		return err
	}
	hdrLen := c.writeFrameHdr(buf, frameData, streamID, seq, len(data))
	return c.commitSendSlotZeroCopy(idx, hdrLen, data, paceBulk)
}

// sendFecFrame sends a parity frame covering count DATA frames starting at
// base: strided G apart for frameFEC "column" groups (seqs base, base+G,
// ..., base+(count-1)*G — see Stream.fecFoldLocked for why groups
// interleave), consecutive for frameFECRow "row" groups. The base seq rides
// in the header's seq field. Unreliable by design: parity is itself
// redundancy, so a lost parity frame just means that group has no FEC
// protection and recovery falls back to NACK/timeout. xorData is copied
// into the send slot's own buffer (it's the stream's live accumulator).
func (c *Connection) sendFecFrame(ftype uint8, streamID, base uint32, count, xorLen int, xorData []byte) error {
	buf, idx, err := c.acquireSendSlot(paceBulk)
	if err != nil {
		return err
	}
	meta := uint32(count)<<16 | uint32(xorLen)
	encodeHeader(buf, ftype, streamID, base, uint32(fecMetaLen+len(xorData)))
	binary.BigEndian.PutUint32(buf[frameHeaderSize:], meta)
	copy(buf[frameHeaderSize+fecMetaLen:], xorData)
	return c.commitSendSlot(idx, frameHeaderSize+fecMetaLen+len(xorData), paceBulk)
}

// sendReliableFrame sends a frame immediately and retransmits it until the
// peer acknowledges its (streamID, seq). Used for FIN and RESET frames,
// which occupy the sequence position after the stream's last data frame.
// Like sendDataFrame, payload is copied once into the retransmit arena and
// referenced directly on the wire rather than copied again.
func (c *Connection) sendReliableFrame(ftype uint8, streamID, seq uint32, payload []byte) error {
	data := c.retransmit.add(ftype, streamID, seq, payload)
	buf, idx, err := c.acquireSendSlot(paceCritical)
	if err != nil {
		return err
	}
	hdrLen := c.writeFrameHdr(buf, ftype, streamID, seq, len(data))
	if err := c.commitSendSlotZeroCopy(idx, hdrLen, data, paceCritical); err != nil {
		return err
	}
	return c.flushSend()
}

// sendWindowUpdate informs the peer of the absolute (cumulative) offset it
// is now allowed to send up to on a stream. Sent unreliably and never
// retransmitted — see windowUpdatePayload for why that's safe; on top of
// that, the current watermark is re-advertised with every ACK flush (see
// flushPendingAcksLocked), so even a lost update only starves the sender's
// window until the next receive batch.
func (c *Connection) sendWindowUpdate(streamID uint32, offset uint64) error {
	if err := c.queueWindowUpdate(streamID, offset); err != nil {
		return err
	}
	return c.flushSend()
}

// queueWindowUpdate queues a WINDOW_UPDATE frame without flushing.
func (c *Connection) queueWindowUpdate(streamID uint32, offset uint64) error {
	buf, idx, err := c.acquireSendSlot(paceCritical)
	if err != nil {
		return err
	}
	encodeHeader(buf, frameWindowUpdate, streamID, 0, 8)
	binary.BigEndian.PutUint64(buf[frameHeaderSize:], offset)
	return c.commitSendSlot(idx, frameHeaderSize+8, paceCritical)
}

// sendControlFrame sends a control frame (no payload) immediately.
func (c *Connection) sendControlFrame(ftype uint8, streamID, seq uint32) error {
	buf, idx, err := c.acquireSendSlot(paceCritical)
	if err != nil {
		return err
	}
	encodeHeader(buf, ftype, streamID, seq, 0)
	if err := c.commitSendSlot(idx, frameHeaderSize, paceCritical); err != nil {
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
	if err := c.queueNackFrame(streamID, seqs); err != nil {
		return err
	}
	return c.flushSend()
}

// sendNackFrames emits one logical gap report across as many existing-format
// NACK frames as necessary, then flushes once. This is wire-compatible with
// older peers and lets one loss-detection firing request the full recovery
// budget instead of silently truncating it to one datagram.
func (c *Connection) sendNackFrames(streamID uint32, seqs []uint32) error {
	if err := forEachSeqListChunk(seqs, c.cfg.maxPayload, func(chunk []uint32) error {
		return c.queueNackFrame(streamID, chunk)
	}); err != nil {
		return err
	}
	return c.flushSend()
}

func (c *Connection) queueNackFrame(streamID uint32, seqs []uint32) error {
	buf, idx, err := c.acquireSendSlot(paceCritical)
	if err != nil {
		return err
	}
	n := encodeSeqListFrame(buf, frameNack, streamID, 0, seqs)
	return c.commitSendSlot(idx, n, paceCritical)
}

// stubbornResendThreshold is the resend attempt from which each further
// retransmission is sent as TWO copies in separate datagrams. A frame on
// its second-or-later resend has already had a retransmission lost, and
// every further loss costs another full recovery cycle (grace + RTT, or a
// whole RTO) during which the frame stalls in-order delivery — the longest
// head-of-line stalls a lossy stream suffers. Doubling only these frames
// adds negligible bandwidth (they're the loss-rate-squared tail) while
// squaring down the chance of yet another cycle.
const stubbornResendThreshold = 2

// queueResendCopy copies one worker-owned retransmission into a connection
// send slot. The slot owns the bytes until sendmmsg/io_uring completes, so
// the worker can immediately return its temporary arena copy afterwards.
func (c *Connection) queueResendCopy(e retransmitEntry) error {
	buf, idx, err := c.acquireSendSlot(paceRecovery)
	if err != nil {
		return err
	}
	hdrLen := c.writeFrameHdr(buf, e.ftype, e.streamID, e.seq, len(e.data))
	copy(buf[hdrLen:], e.data)
	return c.commitSendSlot(idx, hdrLen+len(e.data), paceRecovery)
}

// queueACKFrame queues an ACK frame without flushing, so a flush of many
// ACK frames (one per stream per receive batch — see
// flushPendingAcksLocked) costs one sendmmsg instead of one per frame.
// len(seqs) must not exceed maxAckSeqsPerFrame.
func (c *Connection) queueACKFrame(streamID, cumulative uint32, seqs []uint32) error {
	buf, idx, err := c.acquireSendSlot(paceCritical)
	if err != nil {
		return err
	}
	n := encodeAckFrame(buf, streamID, cumulative, seqs)
	return c.commitSendSlot(idx, n, paceCritical)
}

// acquireSendSlot reserves a message slot in a same-class send batch. If the
// batch is full or belongs to another pacing class, it flushes first. Any GSO
// message being packed is closed so this frame gets its own slot after it,
// preserving queue order.
func (c *Connection) acquireSendSlot(class paceClass) (buf []byte, idx int, err error) {
	c.sendMu.Lock()
	c.closePackLocked()
	if err = c.splitPacingClassLocked(class); err != nil {
		c.sendMu.Unlock()
		return nil, 0, err
	}
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
func (c *Connection) commitSendSlot(idx int, n int, class paceClass) error {
	base := idx * c.iovsPer
	c.sendEP.Iov[base].Len = uint64(n)
	c.sendEP.Hdrs[idx].Iovlen = 1
	c.sendEP.Hdrs[idx].NTransferred = 0
	c.sendLens[idx] = n
	c.sendN = idx + 1
	c.accountDatagramLocked(n, class)

	var err error
	if c.sendN >= c.cfg.batchSize || c.pacingBatchFullLocked() {
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
func (c *Connection) commitSendSlotZeroCopy(idx, hdrLen int, payload []byte, class paceClass) error {
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
	c.accountDatagramLocked(hdrLen+len(payload), class)

	var err error
	if c.sendN >= c.cfg.batchSize || c.pacingBatchFullLocked() {
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
	if c.wirePacer != nil && c.sendDatagrams > 0 && c.sendPaceClass != paceBulk {
		c.closePackLocked()
		if err := c.splitPacingClassLocked(paceBulk); err != nil {
			return err
		}
	}
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
	c.accountDatagramLocked(hdrLen+len(payload), paceBulk)

	full := hdrLen+len(payload) >= c.gsoSize
	if c.packFrames >= c.gsoMaxFrames || !full || c.pacingBatchFullLocked() {
		c.closePackLocked()
		if c.sendN >= c.cfg.batchSize || c.pacingBatchFullLocked() {
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
		c.resetBatchAccountingLocked()
		return nil
	}
	if c.sock.IsShutdown() {
		c.sendN = 0
		c.resetBatchAccountingLocked()
		return ErrClosed
	}

	n := c.sendN
	if c.wirePacer != nil {
		if ok := c.wirePacer.waitBatch(c.sendWireBytes, c.sendUDPBytes, c.sendDatagrams, c.sendPaceClass, c.closeCh); !ok {
			c.sendN = 0
			c.resetBatchAccountingLocked()
			return ErrClosed
		}
	}
	c.sendN = 0
	c.resetBatchAccountingLocked()
	if c.uring != nil {
		return c.flushSendUringLocked(n)
	}
	_, errno := unet.SendMMsgRetry(uintptr(c.fd), c.sendEP.Hdrs[:n], n)
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

// flushSendUringLocked submits the active slot set's n messages as async
// SENDMSG operations and returns without waiting for them: the kernel
// processes batch N on its io-wq workers while the caller builds batch N+1
// in the other slot set. The flip's safety rules:
//
//   - The set being flipped INTO may still have its previous submission in
//     flight, so wait that generation out before returning (normally a free
//     reap — it has had a whole batch's build time to finish).
//   - Arena buffers freed by ACKs can only be recycled once every batch
//     possibly referencing them has completed. A buffer freed before flush
//     k's queuing began is referenced only by batches < k, all complete by
//     the wait above — so each flush releases the entries recorded at the
//     previous flush's watermark, one flush behind their free time.
//
// Frames may complete (and so hit the wire) out of order across a batch;
// like network reordering, the receiver's reorder buffer absorbs it and the
// idempotent ACK/window frames are unaffected. Caller holds sendMu.
func (c *Connection) flushSendUringLocked(n int) error {
	// Small flushes (pings, ACKs, window updates, a lone tail frame) run
	// inline during the submit syscall — identical latency to sendmmsg —
	// because the io-wq wakeup that buys bulk batches their pipelining is
	// pure added latency for a latency-sensitive control frame. Only
	// byte-heavy batches go async.
	const asyncBytesMin = 16 << 10
	total := 0
	for i := 0; i < n; i++ {
		total += c.sendLens[i]
	}
	var sqeFlags uint8
	if total >= asyncBytesMin {
		sqeFlags = iosqeAsync
	}
	set := c.uringSet
	if errno := c.uring.submitSendmsg(c.fd, c.sendEP.Hdrs[:n], set, sqeFlags); errno != 0 {
		return errno
	}
	c.uringSet ^= 1
	c.sendEP = &c.sendEPs[c.uringSet]
	c.sendBufs = c.sendBufSets[c.uringSet]
	if errno := c.uring.waitGen(c.uringSet); errno != 0 {
		return errno
	}
	c.retransmit.drainPendingFreeFirst(c.uringMark)
	c.uringMark = c.retransmit.pendingFreeLen()
	if errno := c.uring.collectErr(); errno != 0 {
		return errno
	}
	return nil
}

// --- background loops ---

// enqueueResends appends de-duplicated retransmit keys to the worker queue.
// It never waits for pacing or network I/O, so it is safe from handleNack
// while recvMu is held.
func (c *Connection) enqueueResends(reqs []retransmitRequest) {
	if len(reqs) == 0 {
		return
	}
	c.resendMu.Lock()
	c.resendQueue = append(c.resendQueue, reqs...)
	c.resendMu.Unlock()
	select {
	case c.resendWake <- struct{}{}:
	default:
	}
}

func (c *Connection) popResend() (retransmitRequest, bool) {
	c.resendMu.Lock()
	if c.resendHead >= len(c.resendQueue) {
		c.resendQueue = c.resendQueue[:0]
		c.resendHead = 0
		c.resendMu.Unlock()
		return retransmitRequest{}, false
	}
	req := c.resendQueue[c.resendHead]
	c.resendQueue[c.resendHead] = retransmitRequest{}
	c.resendHead++
	// Avoid retaining a large burst's backing array forever once most of it
	// has drained, while keeping the steady-state path allocation-free.
	if c.resendHead >= 1024 && c.resendHead*2 >= len(c.resendQueue) {
		copy(c.resendQueue, c.resendQueue[c.resendHead:])
		c.resendQueue = c.resendQueue[:len(c.resendQueue)-c.resendHead]
		c.resendHead = 0
	}
	c.resendMu.Unlock()
	return req, true
}

// serviceResends drains all currently queued recovery work. It revalidates
// each request and takes a worker-owned arena copy before queuing it. The
// completed recovery batch is then granted by the shared pacer during flush;
// an ACK racing with revalidation can cancel a redundant send without risking
// an arena use-after-recycle.
func (c *Connection) serviceResends() bool {
	queued := false
	for {
		req, ok := c.popResend()
		if !ok {
			break
		}
		_, _, live := c.retransmit.peekForResend(req)
		if live {
			e, stillLive := c.retransmit.takeForResend(req)
			if stillLive {
				copies := 1
				if e.retries >= stubbornResendThreshold {
					copies = 2
				}
				for copyNo := 0; copyNo < copies; copyNo++ {
					if c.queueResendCopy(e) != nil {
						break
					}
					queued = true
				}
				c.retransmit.releaseResendCopy(e.data)
				c.statResends.Add(1)
			}
		}
	}
	if queued {
		c.flushSend()
	}
	return queued
}

// retransmitLoop is both the priority resend worker and the periodic RTO
// scanner. Timer and NACK paths enqueue only keys; this goroutine is the
// sole code that may block on pacing and emit retransmissions.
func (c *Connection) retransmitLoop() {
	defer c.doneWg.Done()

	const scanInterval = 20 * time.Millisecond
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()

	for {
		if c.serviceResends() {
			continue
		}
		select {
		case <-c.closeCh:
			return
		case <-c.resendWake:
			continue
		case <-ticker.C:
		}

		c.enqueueResends(c.retransmit.due(c.currentRTO()))
		c.tailNackScan()
		c.gcStreams()
	}
}

// tailNackScan gives every stream a chance to NACK gaps that arrivals will
// never reveal (lost burst tails, fully-lost batches) — see
// Stream.maybeTailNack. Runs on the retransmit loop's tick; per-stream
// grace/throttle checks keep quiet streams nearly free.
func (c *Connection) tailNackScan() {
	now := time.Now()
	c.streamMu.RLock()
	for _, s := range c.streams {
		s.maybeTailNack(now)
	}
	c.streamMu.RUnlock()
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
		if c.closed.Load() {
			c.recvMu.Unlock()
			return
		}
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
	if !c.closed.Load() {
		c.handleDatagramLocked(buf)
	}
	c.recvMu.Unlock()
}

// handleDatagramAndFlush is the Listener fallback's atomic ingress operation.
// A pointer loaded from the listener map while teardown is starting may arrive
// after closed is set; the check under recvMu makes that stale pointer harmless
// and avoids a separate post-close ACK flush.
func (c *Connection) handleDatagramAndFlush(buf []byte) {
	c.recvMu.Lock()
	if !c.closed.Load() {
		c.handleDatagramLocked(buf)
		c.flushPendingAcksLocked()
	}
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
	if c.closed.Load() {
		return
	}
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
		c.handleFec(f, false)
	case frameFECRow:
		if c.cfg.fec2D {
			c.handleFec(f, true)
		}
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
	if !c.closed.Load() {
		c.flushPendingAcksLocked()
	}
	c.recvMu.Unlock()
}

// maxSnapshotFrames caps how many ACK frames one stream's snapshot may
// spread across per flush. One frame's seq list can be smaller than a
// heavily reorder-buffered stream's out-of-order set (a big-window lossy
// link can buffer thousands of frames past a gap); seqs that don't fit go
// un-ACKed and get pointlessly retransmitted at the RTO — duplicate traffic
// exactly when the link is already struggling. Four frames cover ~8k seqs
// per flush and the snapshot's randomized map iteration rotates coverage,
// so anything still unlisted is acknowledged within a few batches, well
// inside the RTO.
const maxSnapshotFrames = 4

// flushPendingAcksLocked sends any accumulated ACKs, chunked to fit the wire
// format. Called after each receive batch, holding recvMu.
//
// Live streams marked dirty this batch get a snapshot ACK — their full
// receive state (cumulative watermark + out-of-order seqs), idempotent and
// re-advertised every flush so lost ACK frames self-heal — chunked across
// up to maxSnapshotFrames frames (each repeating the cumulative watermark,
// which the sender's swept-once accounting makes free), plus a
// WINDOW_UPDATE re-advertising the stream's current flow-control watermark
// so lost grants self-heal the same way. Explicit seq lists (cumulative=0)
// remain for the paths with no live stream state to snapshot:
// dead-stream/tombstone acking and RESET/late-FIN handling. The per-stream
// slices/buffers are truncated in place and retained so steady-state
// flushing allocates nothing; removeStream deletes a stream's slots when
// it goes away.
func (c *Connection) flushPendingAcksLocked() {
	maxAcks := maxAckSeqsPerFrame(c.cfg.maxPayload)
	queued := false

	for streamID := range c.sAckDirty {
		c.streamMu.RLock()
		s := c.streams[streamID]
		c.streamMu.RUnlock()
		if s != nil {
			cum, seqs, granted := s.ackSnapshot(c.ackSnapBuf, maxSnapshotFrames*maxAcks)
			c.ackSnapBuf = seqs[:0]
			rest := seqs
			for {
				n := len(rest)
				if n > maxAcks {
					n = maxAcks
				}
				c.queueACKFrame(streamID, cum, rest[:n])
				rest = rest[n:]
				if len(rest) == 0 {
					break
				}
			}
			c.queueWindowUpdate(streamID, uint64(granted))
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
	if c.closed.Load() {
		return nil, false
	}
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
	if c.closed.Load() || c.streams == nil {
		c.streamMu.Unlock()
		return nil, false
	}
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
	buf, idx, err := c.acquireSendSlot(paceCritical)
	if err != nil {
		return
	}
	n := encodeFrame(buf, frameStreamReset, streamID, 0, resetPayload(code))
	if c.commitSendSlot(idx, n, paceCritical) == nil {
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

// handleFec routes a parity frame (column, or row under WithFEC2D) to its
// stream for possible zero-RTT loss recovery (see Stream.applyFec). Parity
// for unknown/dead streams is silently dropped — it's pure redundancy.
// Caller holds recvMu.
func (c *Connection) handleFec(f frame, row bool) {
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
	if s.applyFec(row, f.seq, count, xorLen, f.payload[fecMetaLen:]) {
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
// already acknowledged, unknown, already queued, or resent within the last
// ¾·SRTT (stale evidence — see requestForResend) are silently skipped.
func (c *Connection) handleNack(f frame) {
	_, seqs, err := decodeSeqListFrame(f, c.ackScratch)
	if err != nil {
		return // discard malformed NACK
	}
	c.ackScratch = seqs[:0]  // retain capacity (shared with handleACK; both run under recvMu)
	minAge := c.srtt * 3 / 4 // srtt: handleNack and updateRTO both run under recvMu
	requests := c.resendScratch[:0]
	for _, seq := range seqs {
		req, ok := c.retransmit.requestForResend(f.streamID, seq, minAge)
		if !ok {
			continue
		}
		requests = append(requests, req)
	}
	c.enqueueResends(requests)
	c.resendScratch = requests[:0]
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

// setSockBuf returns a unet socket option that sets a socket buffer size
// (SO_RCVBUF/SO_SNDBUF) best-effort: the privileged FORCE variant is tried
// first (with CAP_NET_ADMIN it ignores net.core.{r,w}mem_max entirely),
// falling back to the plain option, which the kernel silently clamps to
// the sysctl limit. Unlike unet's SetOptRcvBuf, a smaller-than-requested
// grant is NOT an error: every limit that must respect the real buffer
// size — most importantly the in-flight cap — is derived from the granted
// value read back via getsockopt (see newConnection), so requesting
// generously and taking what the system allows is safe by construction,
// and lets the defaults ask for large buffers without failing outright on
// conservatively-tuned systems.
func setSockBuf(opt, forceOpt, size int) unet.SockOpt {
	return func(s *unet.Socket) error {
		fd, ok := s.Fd.Get()
		if !ok {
			return ErrClosed
		}
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, forceOpt, size); err == nil {
			return nil
		}
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, opt, size) // kernel clamps to the sysctl cap
		return nil
	}
}

func setRcvBuf(size int) unet.SockOpt {
	return setSockBuf(syscall.SO_RCVBUF, syscall.SO_RCVBUFFORCE, size)
}

func setSndBuf(size int) unet.SockOpt {
	return setSockBuf(syscall.SO_SNDBUF, syscall.SO_SNDBUFFORCE, size)
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
	cfg.ensureWirePacer()

	host, port, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}

	sock := unet.NewSocket().
		ResolveFarAddr(host, port).
		ResolveNearAddr("0.0.0.0", 0).
		ConstructUdp().
		SetOpt(setRcvBuf(cfg.recvBufSize)).
		SetOpt(setSndBuf(cfg.sendBufSize)).
		SetOpt(setKernelPacingRate(cfg.wirePacer), cfg.wirePacer == nil).
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
	buf, idx, err := c.acquireSendSlot(paceCritical)
	if err != nil {
		return err
	}
	n := encodeFrame(buf, frameHandshake, 0, 0, payload)
	if err := c.commitSendSlot(idx, n, paceCritical); err != nil {
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
	// The initial timeout and adaptive floor are deliberately independent:
	// start conservatively before the path is known, then permit faster tail
	// recovery once real RTT/jitter samples exist.
	if min := c.cfg.minRetransmitTmout; rto < min {
		rto = min
	}
	c.rtoNs.Store(int64(rto))
	c.rttvarNs.Store(int64(c.rttvar))
	c.srttNs.Store(int64(c.srtt))
}

// inFlightCap returns the current per-stream bound on sent-but-unACKed
// bytes. RTT decides which derived cap applies because RTT decides WHERE
// the window physically lives: on a short-RTT path (loopback, LAN) the
// wire stores almost nothing, so everything the sender has outstanding
// piles into the receiver's socket buffer and the cap must fit its usable
// payload capacity — while on a long-RTT path most of the window is in
// flight, and a cap sized only to the buffer leaves the link's
// bandwidth-delay product unreachable. SRTT starts at 0 (no samples), so a
// connection begins on the conservative LAN cap and steps up within about
// one round trip of real traffic. Safe to call from any goroutine.
func (c *Connection) inFlightCap() int64 {
	// 10ms cleanly separates the regimes: a loaded LAN's queue-inflated
	// samples stay well under it (the estimator keeps the MINIMUM sample
	// per ACK batch, approximating propagation delay), and any path with
	// enough RTT for wire storage to matter sits well above it.
	const wanRTT = 10 * time.Millisecond
	if time.Duration(c.srttNs.Load()) >= wanRTT {
		return c.inFlightCapWan
	}
	return c.inFlightCapLan
}

// currentRTO returns the connection's current adaptive retransmit timeout.
// Safe to call from any goroutine without recvMu.
func (c *Connection) currentRTO() time.Duration {
	return time.Duration(c.rtoNs.Load())
}

// reorderGrace returns how long a receive gap must persist before it's
// treated as loss (NACKed) rather than in-flight reordering. The reorder
// window is characterized by the path's jitter — which rttvar already
// measures — not by the RTO: deriving the grace from the RTO (whose floor
// exists to prevent spurious timer retransmits, a much costlier mistake)
// made every non-FEC-recoverable loss wait out a large fixed delay before
// recovery could even start. 2×rttvar comfortably covers jitter-induced
// reordering; the RTO/reorderGraceFraction ceiling preserves the old
// behavior until real samples exist (rttvar==0) and on wildly-jittery
// paths, and the small floor keeps a quiet link's near-zero rttvar from
// NACKing every io_uring- or NIC-level micro-reordering. A mistaken NACK
// costs one duplicate frame, which the receiver discards — cheap next to a
// stalled gap.
func (c *Connection) reorderGrace() time.Duration {
	rv := c.rttvarNs.Load()
	max := c.currentRTO() / reorderGraceFraction
	if rv == 0 {
		return max
	}
	g := 2 * time.Duration(rv)
	if min := 2 * time.Millisecond; g < min {
		g = min
	}
	if g > max {
		g = max
	}
	return g
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
