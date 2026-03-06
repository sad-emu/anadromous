//go:build linux

package anadromous

import (
	"time"

	"github.com/tredeske/u/unet"
)

const (
	defaultMaxStreams      = 1024
	defaultStreamBufSize   = 256 * 1024 // 256 KB per-stream read buffer
	defaultRecvBufSize     = 4 * 1024 * 1024
	defaultSendBufSize     = 4 * 1024 * 1024
	defaultBatchSize       = 64 // number of messages per recvmmsg/sendmmsg batch
	defaultHandshakeTimout = 5 * time.Second
	defaultKeepAlive       = 10 * time.Second
)

type config struct {
	maxStreams      int
	streamBufSize   int
	recvBufSize     int
	sendBufSize     int
	batchSize       int
	handshakeTimout time.Duration
	keepAlive       time.Duration
	socketOpts      func(*unet.Socket)
}

func defaultConfig() config {
	return config{
		maxStreams:      defaultMaxStreams,
		streamBufSize:   defaultStreamBufSize,
		recvBufSize:     defaultRecvBufSize,
		sendBufSize:     defaultSendBufSize,
		batchSize:       defaultBatchSize,
		handshakeTimout: defaultHandshakeTimout,
		keepAlive:       defaultKeepAlive,
	}
}

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

// WithSocketOptions provides an escape hatch for setting arbitrary unet socket options.
func WithSocketOptions(fn func(*unet.Socket)) Option {
	return func(c *config) { c.socketOpts = fn }
}
