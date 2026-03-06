//go:build linux

package anadromous

// retransmit.go is a the retransmission system

// retransmitEntry would track a single sent frame pending acknowledgement.
type retransmitEntry struct {
	streamID uint32
	seq      uint32
	data     []byte
}

// retransmitQueue would manage pending unacknowledged frames.
type retransmitQueue struct {
	// TODO: implement
}

// TODO: func (c *Connection) enqueueRetransmit(streamID, seq uint32, data []byte)
// TODO: func (c *Connection) ackReceived(streamID, seq uint32)
// TODO: func (c *Connection) retransmitLoop()
