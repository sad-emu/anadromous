//go:build linux

package anadromous

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/tredeske/u/unet"
)

// Listener accepts incoming Connections over UDP.
//
// The listener owns a single unconnected UDP socket. When a new handshake
// arrives from an unknown peer, it creates a new connected UDP socket
// (bound to the same local port via SO_REUSEPORT) for that specific peer,
// and hands off a Connection.
type Listener struct {
	cfg  config
	sock *unet.Socket
	fd   int
	addr unet.Address

	// active connections keyed by remote address string
	connMu sync.RWMutex
	conns  map[string]*Connection

	acceptCh chan *Connection
	closed   atomic.Bool
	closeCh  chan struct{}
	doneWg   sync.WaitGroup
}

// Listen creates a new Listener bound to the given address.
// addr should be "host:port" or ":port".
func Listen(addr string, opts ...Option) (*Listener, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	host, port, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}

	sock := unet.NewSocket().
		ResolveNearAddr(host, port).
		ConstructUdp().
		SetOptReusePort().
		SetOptReuseAddr().
		SetOptRcvBuf(cfg.recvBufSize).
		SetOptSndBuf(cfg.sendBufSize)

	if cfg.socketOpts != nil {
		cfg.socketOpts(sock)
	}

	sock = sock.Bind()
	sock, err = sock.Done()
	if err != nil {
		return nil, err
	}

	fd, valid := sock.Fd.Get()
	if !valid {
		sock.Close()
		return nil, ErrClosed
	}

	l := &Listener{
		cfg:      cfg,
		sock:     sock,
		fd:       fd,
		conns:    make(map[string]*Connection),
		acceptCh: make(chan *Connection, 64),
		closeCh:  make(chan struct{}),
	}
	sock.GetNearAddress(&l.addr)

	l.doneWg.Add(1)
	go l.readLoop()

	return l, nil
}

// Accept waits for and returns an incoming Connection.
func (l *Listener) Accept(ctx context.Context) (*Connection, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, ErrClosed
	case c := <-l.acceptCh:
		return c, nil
	}
}

// Addr returns the listener's local address.
func (l *Listener) Addr() net.Addr { return &l.addr }

// Close stops the listener and closes all connections.
func (l *Listener) Close() error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(l.closeCh)

	l.sock.Shutdown()
	l.doneWg.Wait()
	l.sock.Close()

	l.connMu.Lock()
	for _, c := range l.conns {
		c.Close()
	}
	l.conns = nil
	l.connMu.Unlock()

	return nil
}

// readLoop receives datagrams on the unconnected listener socket.
// Handshakes from new peers create new connections.
// Data for existing connections is forwarded.
func (l *Listener) readLoop() {
	defer l.doneWg.Done()

	buf := make([]byte, maxDatagramSize)

	for {
		if l.closed.Load() {
			return
		}

		n, from, err := syscall.Recvfrom(l.fd, buf, 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if l.closed.Load() {
				return
			}
			continue
		}
		if n < frameHeaderSize {
			continue
		}

		var remoteAddr unet.Address
		remoteAddr.FromSockaddr(from)
		key := remoteAddr.String()

		l.connMu.RLock()
		existing, found := l.conns[key]
		l.connMu.RUnlock()

		if found {
			// Forward to existing connection's read path.
			existing.handleDatagram(buf[:n])
			continue
		}

		// Not an existing connection — must be a handshake.
		f, ferr := decodeFrame(buf[:n])
		if ferr != nil || f.ftype != frameHandshake {
			continue // ignore non-handshake from unknown peers
		}

		connID, herr := decodeHandshake(f.payload)
		if herr != nil {
			continue
		}

		// Create a new connected UDP socket for this peer using SO_REUSEPORT.
		conn, cerr := l.createConnection(from, remoteAddr, connID)
		if cerr != nil {
			continue
		}

		// Send handshake ACK back via the new connected socket.
		conn.sendControlFrame(frameHandshake, 0, 0)

		l.connMu.Lock()
		l.conns[key] = conn
		l.connMu.Unlock()

		select {
		case l.acceptCh <- conn:
		default:
			// Accept channel full — connection will be in map but not delivered.
		}
	}
}

// createConnection builds a new connected UDP socket for a specific remote peer.
// It uses SO_REUSEPORT to bind to the same local address as the listener.
func (l *Listener) createConnection(
	remote syscall.Sockaddr,
	remoteAddr unet.Address,
	connID uint64,
) (*Connection, error) {
	sock := unet.NewSocket().
		SetNearAddr(l.sock.NearAddr).
		SetFarAddr(remote).
		ConstructUdp().
		SetOptReusePort().
		SetOptReuseAddr().
		SetOptRcvBuf(l.cfg.recvBufSize).
		SetOptSndBuf(l.cfg.sendBufSize)

	if l.cfg.socketOpts != nil {
		l.cfg.socketOpts(sock)
	}

	sock = sock.Bind().Connect()
	sock, err := sock.Done()
	if err != nil {
		return nil, err
	}

	fd, valid := sock.Fd.Get()
	if !valid {
		sock.Close()
		return nil, ErrClosed
	}

	c := newConnection(sock, fd, remoteAddr, connID, false, l.cfg)
	c.start()
	return c, nil
}

// removeConnection removes a connection from the listener's tracking map.
func (l *Listener) removeConnection(addr string) {
	l.connMu.Lock()
	delete(l.conns, addr)
	l.connMu.Unlock()
}
