//go:build linux

package anadromous

import (
	"time"

	"github.com/tredeske/u/unet"
)

const (
	defaultMaxStreams        = 1024
	defaultStreamBufSize     = 128 * 1024 * 1024 // 128 MB per-stream read buffer
	defaultRecvBufSize       = 4 * 1024 * 1024
	defaultSendBufSize       = 4 * 1024 * 1024
	defaultBatchSize         = 64 // number of messages per recvmmsg/sendmmsg batch
	defaultHandshakeTimout   = 5 * time.Second
	defaultKeepAlive         = 10 * time.Second
	defaultRetransmitTimeout = 300 * time.Millisecond
	defaultHandshakeRetryIvl = 250 * time.Millisecond
)

type config struct {
	maxStreams      int
	streamBufSize   int
	recvBufSize     int
	sendBufSize     int
	batchSize       int
	maxPayload      int // max frame payload bytes per datagram
	handshakeTimout time.Duration
	keepAlive       time.Duration
	retransmitTmout time.Duration
	bindDevice      string // SO_BINDTODEVICE interface name, empty = unbound
	enableGSO       bool
	maxInFlight     int // per-stream cap on sent-but-unACKed bytes
	socketOpts      func(*unet.Socket)
}

func defaultConfig() config {
	return config{
		maxStreams:      defaultMaxStreams,
		streamBufSize:   defaultStreamBufSize,
		recvBufSize:     defaultRecvBufSize,
		sendBufSize:     defaultSendBufSize,
		batchSize:       defaultBatchSize,
		maxPayload:      defaultMaxPayloadSize,
		handshakeTimout: defaultHandshakeTimout,
		keepAlive:       defaultKeepAlive,
		retransmitTmout: defaultRetransmitTimeout,
		enableGSO:       true,
	}
}

// effectiveMaxInFlight resolves the per-stream cap on sent-but-unACKed
// bytes. An explicit WithMaxBytesInFlight wins; otherwise the cap is twice
// the configured UDP socket receive buffer — the kernel grants sockets 2×
// their SO_RCVBUF request, so this is exactly what a peer provisioned like
// us can absorb with a completely stalled reader. Benchmarking bears the
// coupling out: at 2×recvBufSize the loopback benchmark is drop-free and
// stable, while even 4× reliably tips into the drop→retransmit collapse
// documented on WithMaxBytesInFlight. Raise WithRecvBufferSize (and
// net.core.rmem_max, on both ends) to raise this cap for high-BDP links.
func (c *config) effectiveMaxInFlight() int {
	if c.maxInFlight > 0 {
		return c.maxInFlight
	}
	return 2 * c.recvBufSize
}

// maxDatagram returns the full datagram buffer size for this config.
func (c *config) maxDatagram() int { return frameHeaderSize + c.maxPayload }

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

// WithRecvBufferSize sets the UDP socket receive buffer (SO_RCVBUF).
func WithRecvBufferSize(n int) Option {
	return func(c *config) { c.recvBufSize = n }
}

// WithSendBufferSize sets the UDP socket send buffer (SO_SNDBUF).
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
// adapt it (see Connection.updateRTO), and as a floor afterwards so a burst
// of atypically fast samples can't drive it low enough to cause spurious
// retransmissions.
func WithRetransmitTimeout(d time.Duration) Option {
	return func(c *config) { c.retransmitTmout = d }
}

// WithMaxBytesInFlight caps how many sent-but-unacknowledged bytes a stream
// may have outstanding; Write blocks once the cap is reached. Size it to the
// link's bandwidth-delay product (bytes/sec × round-trip seconds), the same
// way the stream buffer is sized — the cap is additionally bounded above by
// the stream buffer size. When unset it defaults to streamBufSize/8 clamped
// to [8MB, 64MB] — see config.effectiveMaxInFlight.
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

// WithSocketOptions provides an escape hatch for setting arbitrary unet socket options.
func WithSocketOptions(fn func(*unet.Socket)) Option {
	return func(c *config) { c.socketOpts = fn }
}
