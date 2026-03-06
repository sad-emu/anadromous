//go:build linux

// Package anadromous provides a QUIC-like connection/stream multiplexer over UDP
// using the unet library for high-performance Linux networking.
//
// Connections are established between a Listener (server) and a Dial call (client).
// Each Connection multiplexes many bidirectional Streams over a single UDP socket,
// using sendmmsg/recvmmsg for batched I/O.
//
// This library is designed for enterprise use on leased lines where congestion
// control is not required. Throttling is the application's responsibility.
package anadromous
