//go:build linux

package anadromous

// retransmit.go is a stub for the retransmission subsystem.
// It will be implemented to provide reliable delivery over UDP.
//
// Design notes for future implementation:
// - Each sent DATA frame is tracked by (streamID, seq) in a retransmit queue.
// - The receiver sends ACK frames back referencing the (streamID, seq) received.
// - If an ACK is not received within a timeout, the frame is retransmitted.
// - The receiver reorders frames by seq within each stream before delivering.
//
// For now, delivery is best-effort (suitable for leased-line environments
// where packet loss is negligible).

// retransmitEntry would track a single sent frame pending acknowledgement.
type retransmitEntry struct {
	streamID uint32
	seq      uint32
	data     []byte // copy of the datagram
	// TODO: timestamp, retry count, etc.
}

// retransmitQueue would manage pending unacknowledged frames.
type retransmitQueue struct {
	// TODO: implement
}

// TODO: func (c *Connection) enqueueRetransmit(streamID, seq uint32, data []byte)
// TODO: func (c *Connection) ackReceived(streamID, seq uint32)
// TODO: func (c *Connection) retransmitLoop()
