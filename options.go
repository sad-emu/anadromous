//go:build linux

package anadromous

import (
	"time"

	"github.com/tredeske/u/unet"
)

const (
	defaultMaxStreams    = 1024
	defaultStreamBufSize = 128 * 1024 * 1024 // 128 MB per-stream read buffer
	// Socket buffers are requested best-effort (see setSockBuf): the kernel
	// clamps to net.core.{r,w}mem_max without failing, and everything that
	// must respect the real size — the in-flight cap above all — derives
	// from the granted value. So the defaults ask big: on a provisioned
	// system (rmem_max >= 16MB, or CAP_NET_ADMIN) a connection gets 32MB of
	// effective socket buffer and a 24MB default in-flight window —
	// absorbing multi-millisecond receiver stalls and lifting the default
	// BDP ceiling on long-RTT paths — while a conservatively-tuned system
	// just grants what it allows.
	defaultRecvBufSize          = 16 * 1024 * 1024
	defaultSendBufSize          = 16 * 1024 * 1024
	defaultBatchSize            = 64 // number of messages per recvmmsg/sendmmsg batch
	defaultHandshakeTimout      = 5 * time.Second
	defaultKeepAlive            = 10 * time.Second
	defaultRetransmitTimeout    = 300 * time.Millisecond
	defaultMinRetransmitTimeout = 150 * time.Millisecond
	defaultHandshakeRetryIvl    = 250 * time.Millisecond
	defaultFECGroup             = 8 // see WithFEC
)

type config struct {
	maxStreams         int
	streamBufSize      int
	recvBufSize        int
	sendBufSize        int
	batchSize          int
	maxPayload         int // max frame payload bytes per datagram
	handshakeTimout    time.Duration
	keepAlive          time.Duration
	retransmitTmout    time.Duration
	minRetransmitTmout time.Duration
	bindDevice         string // SO_BINDTODEVICE interface name, empty = unbound
	enableGSO          bool
	enableIOUring      bool
	maxInFlight        int   // per-stream cap on sent-but-unACKed bytes
	fecGroup           int   // DATA frames per FEC parity frame; 0 disables FEC
	fec2D              bool  // add row parity (second FEC dimension) — see WithFEC2D
	paceRate           int64 // legacy/private sender pacing rate; 0 = unpaced — see WithPacingRate
	paceBurst          int64
	paceAccounting     WireAccounting
	wirePacer          *WirePacer // optionally shared by many Connections
	socketOpts         func(*unet.Socket)
}

func defaultConfig() config {
	return config{
		maxStreams:         defaultMaxStreams,
		streamBufSize:      defaultStreamBufSize,
		recvBufSize:        defaultRecvBufSize,
		sendBufSize:        defaultSendBufSize,
		batchSize:          defaultBatchSize,
		maxPayload:         defaultMaxPayloadSize,
		handshakeTimout:    defaultHandshakeTimout,
		keepAlive:          defaultKeepAlive,
		retransmitTmout:    defaultRetransmitTimeout,
		minRetransmitTmout: defaultMinRetransmitTimeout,
		enableGSO:          true,
		enableIOUring:      true,
		fecGroup:           defaultFECGroup,
	}
}

// dataCap returns the maximum data bytes per DATA frame: the payload budget
// minus the FEC metadata prefix when FEC is on.
func (c *config) dataCap() int {
	if c.fecGroup > 0 {
		return c.maxPayload - fecMetaLen
	}
	return c.maxPayload
}

// effectiveMaxInFlight resolves the per-stream cap on sent-but-unACKed
// bytes from configuration alone. An explicit WithMaxBytesInFlight wins;
// otherwise the cap is 1.5× the configured UDP socket receive buffer: the
// kernel grants sockets 2× their SO_RCVBUF request, so 2× recvBufSize is
// what a peer provisioned like us can absorb with a completely stalled
// reader — but capping at exactly that leaves zero margin, and
// retransmitted duplicates ride ON TOP of the cap (bytesInFlight counts
// unique unACKed bytes, not wire bytes), so a single drop puts the wire
// volume over the buffer and sustains the drop→retransmit collapse
// documented on WithMaxBytesInFlight. Benchmarking bears the margin out: at
// 2× the loopback benchmark bimodally collapses to ~1/5th throughput, while
// 1.5× is drop-free, stable, and faster at the top end. Raise
// WithRecvBufferSize (and net.core.rmem_max, on both ends) to raise this
// cap for high-BDP links.
//
// newConnection improves on the 2×-doubling assumption by reading the
// actually-granted (rmem_max-clamped) buffer size back from the socket and
// applying the same 25% margin to that; this config-only derivation is the
// fallback when that read fails.
func (c *config) effectiveMaxInFlight() int {
	if c.maxInFlight > 0 {
		return c.maxInFlight
	}
	return c.recvBufSize + c.recvBufSize/2
}

// maxDatagram returns the full datagram buffer size for this config.
func (c *config) maxDatagram() int { return frameHeaderSize + c.maxPayload }

// pacingEnabled reports whether this endpoint has a provisioned-rate pacer.
// The distinction matters to the WAN in-flight-window policy even before a
// Connection has emitted its first batch.
func (c *config) pacingEnabled() bool {
	return c.wirePacer != nil || c.paceRate > 0
}

// ensureWirePacer materializes WithPacingRate's backward-compatible private
// pacer. Dial calls it once per connection; Listener calls it on a fresh config
// copy for each accepted connection. Applications explicitly supplying one
// WirePacer share that pointer instead.
func (c *config) ensureWirePacer() {
	if c.wirePacer != nil || c.paceRate <= 0 {
		return
	}
	c.wirePacer = NewWirePacer(WirePacerConfig{
		RateBytesPerSecond: c.paceRate,
		BurstBytes:         c.paceBurst,
		Accounting:         c.paceAccounting,
	})
}

// Option configures a Listener or Dial.
type Option func(*config)

// WithMaxStreams sets the maximum number of concurrent streams per connection.
func WithMaxStreams(n int) Option {
	return func(c *config) { c.maxStreams = n }
}

// WithStreamBufferSize sets the per-stream receive buffer (and starting
// flow-control window) size in bytes. Streams start at this size immediately
// rather than growing into it: this protocol has no congestion control by
// design (WAN links with known, manually-tuned capacity), so size this to
// the link's bandwidth-delay product rather than treating it as a cautious
// upper bound — a stream allocates this much memory up front, multiplied by
// however many concurrent streams a connection opens (see WithMaxStreams).
func WithStreamBufferSize(n int) Option {
	return func(c *config) { c.streamBufSize = n }
}

// WithRecvBufferSize sets the UDP socket receive buffer (SO_RCVBUF)
// request. Best-effort: without CAP_NET_ADMIN the kernel clamps the request
// to net.core.rmem_max rather than failing, and the connection's derived
// limits (the in-flight cap in particular) follow the granted size, not the
// request — so raising this only takes full effect once rmem_max allows it.
func WithRecvBufferSize(n int) Option {
	return func(c *config) { c.recvBufSize = n }
}

// WithSendBufferSize sets the UDP socket send buffer (SO_SNDBUF) request.
// Best-effort like WithRecvBufferSize (clamped to net.core.wmem_max).
func WithSendBufferSize(n int) Option {
	return func(c *config) { c.sendBufSize = n }
}

// WithBatchSize sets the number of messages per sendmmsg/recvmmsg batch.
func WithBatchSize(n int) Option {
	return func(c *config) { c.batchSize = n }
}

// WithHandshakeTimeout sets the maximum duration for the handshake to complete.
func WithHandshakeTimeout(d time.Duration) Option {
	return func(c *config) { c.handshakeTimout = d }
}

// WithKeepAlive sets the keep-alive ping interval. Zero disables.
func WithKeepAlive(d time.Duration) Option {
	return func(c *config) { c.keepAlive = d }
}

// WithIdleTimeout sets how long a silent peer is tolerated before the
// connection is closed. Internally this drives the keep-alive ping interval
// (d/3, with the connection declared dead after 3 missed pongs), so a dead
// peer is detected within roughly d. Zero disables idle detection.
// Equivalent to quic-go's MaxIdleTimeout.
func WithIdleTimeout(d time.Duration) Option {
	return func(c *config) { c.keepAlive = d / 3 }
}

// WithMaxDatagramSize sets the maximum UDP datagram size (frame header plus
// payload) used for all frames on the connection. Both endpoints of a
// connection MUST be configured with the same value: this library targets
// links whose MTU is known, and does no path-MTU discovery. Values must
// exceed the frame header overhead; anything else is ignored.
// Equivalent to quic-go's InitialPacketSize.
func WithMaxDatagramSize(n int) Option {
	return func(c *config) {
		if n > frameHeaderSize+8 {
			c.maxPayload = n - frameHeaderSize
		}
	}
}

// WithBindToDevice binds the UDP socket to a network interface via
// SO_BINDTODEVICE (Linux). Requires CAP_NET_RAW or root.
func WithBindToDevice(ifname string) Option {
	return func(c *config) { c.bindDevice = ifname }
}

// WithRetransmitTimeout sets the initial interval a DATA frame waits for an
// ACK before being retransmitted, used until enough RTT samples arrive to
// adapt it (see Connection.updateRTO). The adaptive estimator's floor is
// configured independently with WithMinRetransmitTimeout.
func WithRetransmitTimeout(d time.Duration) Option {
	return func(c *config) { c.retransmitTmout = d }
}

// WithMinRetransmitTimeout sets the floor for the adaptive retransmission
// timeout after RTT samples are available. Keeping this separate from the
// initial timeout lets a connection start conservatively while converging to
// faster recovery on a stable low-jitter path. The default is 150ms.
func WithMinRetransmitTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.minRetransmitTmout = d
		}
	}
}

// WithMaxBytesInFlight caps how many sent-but-unacknowledged bytes a stream
// may have outstanding; Write blocks once the cap is reached. Size it to the
// link's bandwidth-delay product (bytes/sec × round-trip seconds), the same
// way the stream buffer is sized — the cap is additionally bounded above by
// the stream buffer size. When unset it defaults to 1.5× the socket receive
// buffer size — see config.effectiveMaxInFlight.
//
// This cap is the protocol's only brake, so don't set it huge "to be safe":
// with no congestion control by design, everything outstanding is re-blasted
// every retransmit interval when things go wrong. If the cap is much larger
// than what the path and receiver can actually absorb, one drop burst (e.g.
// the receiver's UDP socket buffer overflowing) snowballs — retransmits of
// a huge window add more load, causing more drops — into a persistent
// collapse that a BDP-sized cap makes self-limiting instead.
func WithMaxBytesInFlight(n int) Option {
	return func(c *config) { c.maxInFlight = n }
}

// WithGSO controls UDP generic segmentation offload on the send path: up to
// seven full-size DATA frames are packed into one sendmmsg message that the
// kernel segments into individual datagrams, cutting per-frame send cost —
// and, symmetrically, a GRO-capable receiver ingests the burst as one
// coalesced buffer. On by default (silently disabled on kernels without
// UDP_SEGMENT/UDP_GRO support, pre-4.18/5.0): benchmarked on loopback it
// both raises throughput (~6GB/s vs ~4.5GB/s unpacked) and stabilizes it,
// because the receiver does a fraction of the per-datagram work and so
// stalls — whose packet drops snowball on a protocol with no congestion
// control — become rarer. Note the burst-vs-receiver-buffer interaction
// documented on WithMaxBytesInFlight applies regardless of this setting;
// the in-flight cap is what bounds bursts, not GSO. Receivers need no
// matching option — the receive side always accepts both packed and
// unpacked senders.
func WithGSO(enabled bool) Option {
	return func(c *config) { c.enableGSO = enabled }
}

// WithIOUring controls the pipelined io_uring send path: each flushed batch
// is submitted as async SENDMSG operations that the kernel's io-wq workers
// process while the application builds the next batch, instead of the
// caller blocking in sendmmsg until every datagram has been handed to the
// network stack. On by default; silently falls back to sendmmsg on kernels
// without io_uring (or without IORING_FEAT_SINGLE_MMAP, pre-5.4), and in
// environments that deny the io_uring syscalls entirely (some container
// seccomp profiles). Wire format and receive path are unaffected — this is
// purely a send-side execution strategy.
func WithIOUring(enabled bool) Option {
	return func(c *config) { c.enableIOUring = enabled }
}

// WithFEC sets how many DATA frames are covered by each XOR parity frame
// (forward error correction), or 0 to disable. Default 8 (12.5% bandwidth
// overhead). With FEC, any single lost frame in a group is reconstructed by
// the receiver the moment the group's parity arrives — zero round trips —
// instead of costing a NACK or retransmit-timeout cycle. On a
// high-loss/high-RTT link, recovery latency is what throughput dies of, so
// trading spare bandwidth for it is exactly this protocol's design bargain
// (known-capacity links, no congestion control). Two or more losses within
// one group still fall back to NACK/timeout recovery; groups also flush,
// possibly short, when the stream's write side closes.
//
// BOTH ENDS MUST USE THE SAME SETTING (like WithMaxDatagramSize): enabling
// FEC adds a 4-byte metadata prefix to every DATA frame's wire payload, so
// mismatched ends cannot parse each other's data frames at all. Group size
// is clamped to [2, 32].
func WithFEC(groupSize int) Option {
	return func(c *config) {
		switch {
		case groupSize <= 0:
			c.fecGroup = 0
		case groupSize < 2:
			c.fecGroup = 2
		case groupSize > 32:
			c.fecGroup = 32
		default:
			c.fecGroup = groupSize
		}
	}
}

// WithFEC2D adds a second, orthogonal FEC dimension on top of WithFEC: as
// well as the strided "column" parity groups, a parity frame is emitted
// over every G consecutive DATA frames (a "row"). Columns absorb burst
// loss (a dropped GSO super-packet puts at most one loss per column); rows
// recover the random double losses that defeat a single column — and the
// two dimensions peel iteratively, each recovery potentially unblocking
// the other, so most multi-loss patterns short of a full G×G stopping set
// reconstruct with zero round trips. Costs a second 1/G of bandwidth
// (12.5% more at the default group size, 25% total) — the same
// spare-bandwidth-for-latency bargain as WithFEC, for links lossy enough
// that double losses per group are common (e.g. ≥5-10% loss).
//
// BOTH ENDS SHOULD USE THE SAME SETTING: a receiver without it ignores row
// parity frames (wasting their bandwidth), and a receiver with it folds
// row state that a non-sending peer never uses. Requires WithFEC > 0.
func WithFEC2D(enabled bool) Option {
	return func(c *config) { c.fec2D = enabled }
}

// WithPacingRate sets each connection's send rate in bytes per second — size
// it to the link's provisioned capacity. Applications pooling multiple
// connections over one circuit should instead create one WirePacer and pass
// it with WithWirePacer to every Dial or to the Listener.
//
// The legacy accounting is encoded UDP payload bytes. Use
// WithPacingAccounting, or construct a shared WirePacer, to include IP,
// Ethernet, carrier encapsulation and minimum-frame padding.
// An UNPACED sender transmits in line-rate bursts (as fast as the CPU and
// NIC go), and whenever the path's bottleneck is slower than that, its
// queue overflows and drops — self-inflicted loss that no recovery scheme
// can outrun. Pacing removes the self-inflicted component entirely;
// random path loss is then absorbed by FEC/NACK recovery without the rate
// ever dipping.
//
// Retransmissions and FEC parity spend from the same budget (retransmits are
// granted ahead of new bulk traffic across a shared pacer), so the wire rate stays at the
// configured value rather than ballooning past it under loss; new data
// slows by exactly the overhead being spent on recovery, no more.
//
// Setting a rate also unlocks the uncapped long-RTT send window (bounded
// only by flow control / WithStreamBufferSize instead of the socket
// buffer), which is what lets throughput reach rate×RTT products far past
// the socket buffer. Zero (the default) disables pacing AND keeps the
// bounded window: unpaced line-rate bursts into a slower bottleneck cause
// self-inflicted drop storms, and with an uncapped window those play out
// as multi-second Write stalls.
func WithPacingRate(bytesPerSec int) Option {
	return func(c *config) {
		if bytesPerSec < 0 {
			bytesPerSec = 0
		}
		c.paceRate = int64(bytesPerSec)
		c.wirePacer = nil
	}
}

// WithPacingAccounting configures carrier-visible bytes charged in addition
// to each encoded UDP payload when WithPacingRate constructs its pacer.
func WithPacingAccounting(accounting WireAccounting) Option {
	return func(c *config) { c.paceAccounting = accounting }
}

// WithPacingBurstBytes overrides the default two-millisecond batch/token
// quantum used by WithPacingRate. Values <= 0 select the default.
func WithPacingBurstBytes(bytes int64) Option {
	return func(c *config) { c.paceBurst = bytes }
}

// WithWirePacer injects a caller-owned pacer. Reusing the same pointer across
// Dial calls is what enforces one aggregate wire budget across a connection
// pool. The pacer contains no goroutine and needs no Close call.
func WithWirePacer(pacer *WirePacer) Option {
	return func(c *config) {
		c.wirePacer = pacer
		c.paceRate = 0
	}
}

// WithSocketOptions provides an escape hatch for setting arbitrary unet socket options.
func WithSocketOptions(fn func(*unet.Socket)) Option {
	return func(c *config) { c.socketOpts = fn }
}
