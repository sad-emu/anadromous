//go:build linux

package anadromous

import (
	"container/heap"
	"math/rand"
	"net"
	"sync"
	"time"
)

// lossy_test.go provides an in-process hostile-network simulator: a UDP
// proxy that applies drop/delay/jitter/duplication to datagrams in both
// directions, approximating the netem profile used by salmon-cannon's
// netem_test.sh (50ms ±10ms delay, 10% loss, dup) without needing root or
// tc. Deterministically seeded so lossy benchmark runs are comparable.

type lossyProfile struct {
	dropProb float64       // per-datagram drop probability, each direction
	dupProb  float64       // per-datagram duplication probability
	delay    time.Duration // one-way base delay
	jitter   time.Duration // one-way delay jitter (normal distribution std-dev); creates reordering
	seed     int64
}

// netemLikeProfile mirrors netem_test.sh's hostile profile (minus corrupt:
// the protocol has no frame checksum yet, so corruption testing would
// measure undefined behavior rather than recovery).
func netemLikeProfile() lossyProfile {
	return lossyProfile{
		dropProb: 0.10,
		dupProb:  0.05,
		delay:    50 * time.Millisecond,
		jitter:   10 * time.Millisecond,
		seed:     42,
	}
}

type lossyPacket struct {
	data      []byte
	deliverAt time.Time
}

type lossyPacketHeap []lossyPacket

func (h lossyPacketHeap) Len() int           { return len(h) }
func (h lossyPacketHeap) Less(i, j int) bool { return h[i].deliverAt.Before(h[j].deliverAt) }
func (h lossyPacketHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *lossyPacketHeap) Push(x any)        { *h = append(*h, x.(lossyPacket)) }
func (h *lossyPacketHeap) Pop() any {
	old := *h
	n := len(old)
	p := old[n-1]
	old[n-1] = lossyPacket{}
	*h = old[:n-1]
	return p
}

// lossyProxy relays UDP datagrams between a client and a server address,
// applying the profile in both directions.
type lossyProxy struct {
	profile    lossyProfile
	clientSide *net.UDPConn // clients dial this
	serverSide *net.UDPConn // connected to the real server

	mu         sync.Mutex
	clientAddr *net.UDPAddr // learned from the first client datagram

	closed chan struct{}
}

func newLossyProxy(serverAddr string, profile lossyProfile) (*lossyProxy, error) {
	srvAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return nil, err
	}
	clientSide, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	serverSide, err := net.DialUDP("udp", nil, srvAddr)
	if err != nil {
		clientSide.Close()
		return nil, err
	}
	// Large socket buffers so the proxy itself isn't the drop source beyond
	// the profile (bounded by net.core.{r,w}mem_max).
	clientSide.SetReadBuffer(32 << 20)
	clientSide.SetWriteBuffer(32 << 20)
	serverSide.SetReadBuffer(32 << 20)
	serverSide.SetWriteBuffer(32 << 20)

	p := &lossyProxy{
		profile:    profile,
		clientSide: clientSide,
		serverSide: serverSide,
		closed:     make(chan struct{}),
	}

	toServer := p.startDeliverer(func(data []byte) {
		p.serverSide.Write(data)
	}, profile.seed)
	toClient := p.startDeliverer(func(data []byte) {
		p.mu.Lock()
		addr := p.clientAddr
		p.mu.Unlock()
		if addr != nil {
			p.clientSide.WriteToUDP(data, addr)
		}
	}, profile.seed+1)

	go p.pump(clientSide, nil, toServer)
	go p.pump(nil, serverSide, toClient)
	return p, nil
}

// Addr returns the address clients should dial.
func (p *lossyProxy) Addr() string { return p.clientSide.LocalAddr().String() }

func (p *lossyProxy) Close() {
	close(p.closed)
	p.clientSide.Close()
	p.serverSide.Close()
}

// pump reads datagrams from one side and hands them to a deliverer.
func (p *lossyProxy) pump(fromClient *net.UDPConn, fromServer *net.UDPConn, out chan<- []byte) {
	buf := make([]byte, 65536)
	for {
		var n int
		var err error
		if fromClient != nil {
			var addr *net.UDPAddr
			n, addr, err = fromClient.ReadFromUDP(buf)
			if err == nil {
				p.mu.Lock()
				p.clientAddr = addr
				p.mu.Unlock()
			}
		} else {
			n, err = fromServer.Read(buf)
		}
		if err != nil {
			select {
			case <-p.closed:
				return
			default:
			}
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case out <- data:
		case <-p.closed:
			return
		}
	}
}

// startDeliverer runs one direction's impairment stage: it applies drop,
// duplication, and jittered delay (which naturally reorders), holding
// packets in a deliver-time heap.
func (p *lossyProxy) startDeliverer(send func([]byte), seed int64) chan<- []byte {
	in := make(chan []byte, 4096)
	go func() {
		rng := rand.New(rand.NewSource(seed))
		var pending lossyPacketHeap
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()

		schedule := func(data []byte) {
			if rng.Float64() < p.profile.dropProb {
				return
			}
			copies := 1
			if rng.Float64() < p.profile.dupProb {
				copies = 2
			}
			for i := 0; i < copies; i++ {
				d := p.profile.delay + time.Duration(rng.NormFloat64()*float64(p.profile.jitter))
				if d < 0 {
					d = 0
				}
				heap.Push(&pending, lossyPacket{data: data, deliverAt: time.Now().Add(d)})
			}
		}

		for {
			var wake <-chan time.Time
			if len(pending) > 0 {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(time.Until(pending[0].deliverAt))
				wake = timer.C
			}
			select {
			case <-p.closed:
				return
			case data := <-in:
				schedule(data)
			case <-wake:
				now := time.Now()
				for len(pending) > 0 && !pending[0].deliverAt.After(now) {
					pkt := heap.Pop(&pending).(lossyPacket)
					send(pkt.data)
				}
			}
		}
	}()
	return in
}
