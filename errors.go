//go:build linux

package anadromous

import "errors"

var (
	// ErrClosed is returned when operating on a closed connection or stream.
	ErrClosed = errors.New("anadromous: closed")

	// ErrStreamClosed is returned when reading/writing a closed stream.
	ErrStreamClosed = errors.New("anadromous: stream closed")

	// ErrStreamFIN is returned when writing to a half-closed (FIN'd) stream.
	ErrStreamFIN = errors.New("anadromous: stream FIN sent")

	// ErrMaxStreams is returned when the stream limit is reached.
	ErrMaxStreams = errors.New("anadromous: max streams exceeded")

	// ErrConnRefused is returned when a handshake is not accepted.
	ErrConnRefused = errors.New("anadromous: connection refused")

	// ErrHandshakeTimeout is returned when the handshake doesn't complete in time.
	ErrHandshakeTimeout = errors.New("anadromous: handshake timeout")

	// ErrPayloadTooLarge is returned when a write exceeds maxPayloadSize.
	ErrPayloadTooLarge = errors.New("anadromous: payload too large for single datagram")
)
