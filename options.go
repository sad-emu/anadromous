//go:build linux

package anadromous

import (
	"time"

	"github.com/tredeske/u/unet"
)

const (
	defaultMaxStreams        = 1024
	defaultStreamBufSize     = 256 * 1024 // 256 KB per-stream read buffer
	defaultRecvBufSize       = 4 * 1024 * 1024
	defaultSendBufSize       = 4 * 1024 * 1024
	defaultBatchSize         = 64 // number of messages per recvmmsg/sendmmsg batch
	defaultHandshakeTimout   = 5 * time.Second
	defaultKeepAlive         = 10 * time.Second
	defaultRetransmitTimeout = 300 * time.Millisecond
	defaultRetransmitRetries = 15
	defaultHandshakeRetryIvl = 250 * time.Millisecond
)

type config struct {
	maxStreams        int
	streamBufSize     int
	recvBufSize       int
	sendBufSize       int
	batchSize         int
	maxPayload        int // max frame payload bytes per datagram
	handshakeTimout   time.Duration
	keepAlive         time.Duration
	retransmitTmout   time.Duration
	retransmitRetries int
	bindDevice        string // SO_BINDTODEVICE interface name, empty = unbound
	socketOpts        func(*unet.Socket)
}

func defaultConfig() config {
	return config{
		maxStreams:        defaultMaxStreams,
		streamBufSize:     defaultStreamBufSize,
		recvBufSize:       defaultRecvBufSize,
		sendBufSize:       defaultSendBufSize,
		batchSize:         defaultBatchSize,
		maxPayload:        defaultMaxPayloadSize,
		handshakeTimout:   defaultHandshakeTimout,
		keepAlive:         defaultKeepAlive,
		retransmitTmout:   defaultRetransmitTimeout,
		retransmitRetries: defaultRetransmitRetries,
	}
}

// maxDatagram returns the full datagram buffer size for this config.
func (c *config) maxDatagram() int { return frameHeaderSize + c.maxPayload }

// Option configures a Listener or Dial.
type Option func(*config)

// WithMaxStreams sets the maximum number of concurrent streams per connection.
func WithMaxStreams(n int) Option {
	return func(c *config) { c.maxStreams = n }
}

// WithStreamBufferSize sets the per-stream receive buffer size in bytes.
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

// WithRetransmitTimeout sets the fixed interval a DATA frame waits for an ACK
// before being retransmitted. There is no backoff: this interval never grows.
func WithRetransmitTimeout(d time.Duration) Option {
	return func(c *config) { c.retransmitTmout = d }
}

// WithRetransmitMaxRetries sets how many times a DATA frame is retransmitted
// before its stream is failed with ErrRetransmitExceeded.
func WithRetransmitMaxRetries(n int) Option {
	return func(c *config) { c.retransmitRetries = n }
}

// WithSocketOptions provides an escape hatch for setting arbitrary unet socket options.
func WithSocketOptions(fn func(*unet.Socket)) Option {
	return func(c *config) { c.socketOpts = fn }
}
