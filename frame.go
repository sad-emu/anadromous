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
	frameWindowUpdate uint8 = 0x05 // Flow control: absolute (cumulative) send watermark
	framePing         uint8 = 0x06 // Keep-alive ping
	framePong         uint8 = 0x07 // Ping response
	frameGoAway       uint8 = 0x08 // Connection shutting down
	frameACK          uint8 = 0x09 // Acknowledge received data
	frameHandshake    uint8 = 0x0A // Connection handshake
	frameStreamReset  uint8 = 0x0B // Abort the sender's write side (like QUIC RESET_STREAM)
	frameStopSending  uint8 = 0x0C // Ask the peer to stop sending (like QUIC STOP_SENDING)
	frameNack         uint8 = 0x0D // Fast retransmit: resend specific (stream, seq...) frames now; seq-list payload like ACK
	frameFEC          uint8 = 0x0E // Forward-error-correction parity over a group of DATA frames
)

// fecMetaLen is the per-frame metadata prefix present on DATA and FEC frame
// payloads when FEC is enabled (see WithFEC): payload = [meta(4) | data].
// For DATA frames meta is reserved (0). For FEC frames, seq carries the
// group index k (covering DATA seqs [k*G, k*G+count)) and meta packs
// count<<16 | xorOfMemberLengths; data is the XOR of the members' data,
// each padded to the group's max length. Carrying the prefix on data frames
// too (rather than only on parity) keeps full DATA and full parity frames
// the same wire size, which the GSO packing path requires — the data cap
// shrinks by 4 bytes instead.
const fecMetaLen = 4

// frameHeaderSize is the fixed size of a frame header on the wire.
// Layout: [Type(1) | StreamID(4) | Sequence(4) | Length(4)] = 13 bytes
const frameHeaderSize = 13

// defaultMaxPayloadSize is the default maximum payload per UDP datagram.
// Conservative: 1200 bytes to fit within common MTUs without fragmentation.
// Configurable per connection via WithMaxDatagramSize.
const defaultMaxPayloadSize = 1200

// defaultMaxDatagramSize is the default full datagram size (header + payload).
const defaultMaxDatagramSize = frameHeaderSize + defaultMaxPayloadSize

// maxAckSeqsPerFrame returns the maximum number of sequence numbers that fit
// in a single ACK/NACK frame's payload for a given max payload size:
// [Cumulative(4) | Count(4) | Seq(4) x N] <= maxPayload.
func maxAckSeqsPerFrame(maxPayload int) int {
	return (maxPayload - 8) / 4
}

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

// encodeSeqListFrame writes a complete seq-list frame (header + payload)
// into buf; ACK and NACK share this format. The stream is carried in the
// generic header's StreamID field; the payload is
// [Cumulative(4) | Count(4) | Seq1(4) | Seq2(4) | ...].
//
// For ACK, Cumulative=C means "every DATA seq below C has been received"
// and the list holds seqs received beyond C (out-of-order frames, plus the
// FIN seq once seen); an ACK is therefore an idempotent snapshot of receive
// state, re-advertised on every flush, so a lost ACK frame is healed by the
// next one instead of silently un-acknowledging a whole receive batch until
// the retransmit timer re-sends it — the same reasoning as TCP cumulative
// ACKs / QUIC ACK ranges. Cumulative=0 makes the frame an explicit-list-only
// ACK (used for streams with no live receive state: tombstones, resets).
// For NACK the Cumulative field is unused (0) and the list holds the seqs
// to resend. len(seqs) must not exceed maxAckSeqsPerFrame.
func encodeSeqListFrame(buf []byte, ftype uint8, streamID, cumulative uint32, seqs []uint32) int {
	plen := uint32(8 + 4*len(seqs))
	encodeHeader(buf, ftype, streamID, 0, plen)
	binary.BigEndian.PutUint32(buf[frameHeaderSize:frameHeaderSize+4], cumulative)
	binary.BigEndian.PutUint32(buf[frameHeaderSize+4:frameHeaderSize+8], uint32(len(seqs)))
	for i, seq := range seqs {
		off := frameHeaderSize + 8 + 4*i
		binary.BigEndian.PutUint32(buf[off:off+4], seq)
	}
	return frameHeaderSize + int(plen)
}

// encodeAckFrame writes a complete ACK frame into buf.
func encodeAckFrame(buf []byte, streamID, cumulative uint32, seqs []uint32) int {
	return encodeSeqListFrame(buf, frameACK, streamID, cumulative, seqs)
}

// decodeSeqListFrame extracts the cumulative watermark and explicit
// sequence numbers from a decoded ACK/NACK frame's payload (see
// encodeSeqListFrame for the semantics). The stream is f.streamID. Decodes
// into scratch's backing array when it has the capacity, so a caller
// processing a steady flow of frames can reuse one buffer instead of
// allocating per frame.
func decodeSeqListFrame(f frame, scratch []uint32) (cumulative uint32, seqs []uint32, err error) {
	if len(f.payload) < 8 {
		err = errors.New("seq-list payload too small")
		return
	}
	cumulative = binary.BigEndian.Uint32(f.payload[0:4])
	count := int(binary.BigEndian.Uint32(f.payload[4:8]))
	need := 8 + 4*count
	if len(f.payload) < need {
		err = fmt.Errorf("seq-list payload truncated: have %d, need %d", len(f.payload), need)
		return
	}
	if cap(scratch) >= count {
		seqs = scratch[:count]
	} else {
		seqs = make([]uint32, count)
	}
	for i := range seqs {
		off := 8 + 4*i
		seqs[i] = binary.BigEndian.Uint32(f.payload[off : off+4])
	}
	return
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

// windowUpdatePayload encodes a WINDOW_UPDATE frame payload carrying an
// absolute (cumulative) flow-control watermark: the total number of bytes
// the peer is allowed to have sent on this stream since it opened.
//
// This is deliberately cumulative rather than a delta, and the frame is sent
// unreliably (never retransmitted). A delta would be lost forever if its
// datagram were dropped, permanently starving the sender's window on a lossy
// link. A cumulative watermark is idempotent: applying an equal or smaller
// value is a no-op (see Stream.setMaxSendOffset), so a dropped update is
// simply superseded by the next, larger one — self-healing under loss with
// no retransmission machinery needed, the same trick QUIC's MAX_STREAM_DATA
// uses.
func windowUpdatePayload(offset uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, offset)
	return b
}

// decodeWindowUpdatePayload extracts the absolute watermark from a
// WINDOW_UPDATE frame's payload.
func decodeWindowUpdatePayload(payload []byte) (offset uint64, err error) {
	if len(payload) < 8 {
		err = errors.New("window update payload too small")
		return
	}
	offset = binary.BigEndian.Uint64(payload)
	return
}

// resetPayload encodes a RESET_STREAM frame payload carrying the application
// error code.
func resetPayload(code uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, code)
	return b
}

// decodeResetPayload extracts the application error code from a RESET_STREAM
// payload. A missing/short payload decodes as code 0.
func decodeResetPayload(payload []byte) uint64 {
	if len(payload) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(payload)
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
