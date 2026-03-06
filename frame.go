//go:build linux

package anadromous

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frame types for the wire protocol.
const (
	frameData         uint8 = 0x01 // Stream payload
	frameStreamOpen   uint8 = 0x02 // Open a new stream
	frameStreamClose  uint8 = 0x03 // Graceful close (both directions)
	frameStreamFIN    uint8 = 0x04 // Half-close (sender done writing)
	frameWindowUpdate uint8 = 0x05 // Flow control window increase
	framePing         uint8 = 0x06 // Keep-alive ping
	framePong         uint8 = 0x07 // Ping response
	frameGoAway       uint8 = 0x08 // Connection shutting down
	frameACK          uint8 = 0x09 // Acknowledge received data
	frameHandshake    uint8 = 0x0A // Connection handshake
)

// frameHeaderSize is the fixed size of a frame header on the wire.
// Layout: [Type(1) | StreamID(4) | Sequence(4) | Length(4)] = 13 bytes
const frameHeaderSize = 13

// maxPayloadSize is the maximum payload per UDP datagram.
// Conservative default: 1200 bytes to fit within common MTUs without fragmentation.
const maxPayloadSize = 1200

// maxDatagramSize is the max full datagram size (header + payload).
const maxDatagramSize = frameHeaderSize + maxPayloadSize

// frame represents a decoded frame header plus its payload.
type frame struct {
	ftype    uint8
	streamID uint32
	seq      uint32
	length   uint32
	payload  []byte
}

// encodeHeader writes the frame header into buf (must be >= frameHeaderSize).
// Returns frameHeaderSize.
func encodeHeader(buf []byte, ftype uint8, streamID, seq, length uint32) {
	buf[0] = ftype
	binary.BigEndian.PutUint32(buf[1:5], streamID)
	binary.BigEndian.PutUint32(buf[5:9], seq)
	binary.BigEndian.PutUint32(buf[9:13], length)
}

// encodeFrame writes a complete frame (header + payload) into buf.
// Returns the total number of bytes written.
func encodeFrame(buf []byte, ftype uint8, streamID, seq uint32, payload []byte) int {
	plen := uint32(len(payload))
	encodeHeader(buf, ftype, streamID, seq, plen)
	copy(buf[frameHeaderSize:], payload)
	return frameHeaderSize + int(plen)
}

// decodeHeader reads a frame header from buf (must be >= frameHeaderSize).
func decodeHeader(buf []byte) (ftype uint8, streamID, seq, length uint32, err error) {
	if len(buf) < frameHeaderSize {
		err = errors.New("buffer too small for frame header")
		return
	}
	ftype = buf[0]
	streamID = binary.BigEndian.Uint32(buf[1:5])
	seq = binary.BigEndian.Uint32(buf[5:9])
	length = binary.BigEndian.Uint32(buf[9:13])
	return
}

// decodeFrame decodes a full frame from a datagram buffer.
func decodeFrame(buf []byte) (f frame, err error) {
	f.ftype, f.streamID, f.seq, f.length, err = decodeHeader(buf)
	if err != nil {
		return
	}
	total := frameHeaderSize + int(f.length)
	if len(buf) < total {
		err = fmt.Errorf("frame payload truncated: have %d, need %d", len(buf)-frameHeaderSize, f.length)
		return
	}
	if f.length > 0 {
		f.payload = buf[frameHeaderSize:total]
	}
	return
}

// handshakePayload encodes a handshake frame payload containing the connection ID.
func handshakePayload(connID uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, connID)
	return b
}

// decodeHandshake extracts the connection ID from a handshake payload.
func decodeHandshake(payload []byte) (connID uint64, err error) {
	if len(payload) < 8 {
		err = errors.New("handshake payload too small")
		return
	}
	connID = binary.BigEndian.Uint64(payload)
	return
}
