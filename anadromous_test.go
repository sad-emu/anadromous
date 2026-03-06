//go:build linux

package anadromous

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestStreamSend1K(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	t.Logf("listener on %s", addr)

	const dataSize = 1024
	sendData := make([]byte, dataSize)
	if _, err := rand.Read(sendData); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	errCh := make(chan error, 1)
	recvCh := make(chan []byte, 1)

	// Server goroutine
	go func() {
		conn, aerr := ln.Accept(ctx)
		if aerr != nil {
			errCh <- fmt.Errorf("server Accept: %w", aerr)
			return
		}
		defer conn.Close()

		stream, serr := conn.AcceptStream(ctx)
		if serr != nil {
			errCh <- fmt.Errorf("server AcceptStream: %w", serr)
			return
		}
		defer stream.Close()

		buf := make([]byte, dataSize*2)
		total := 0
		for total < dataSize {
			n, rerr := stream.Read(buf[total:])
			total += n
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				errCh <- fmt.Errorf("server Read: %w", rerr)
				return
			}
		}
		recvCh <- buf[:total]
	}()

	// Client
	clientConn, err := Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clientConn.Close()

	time.Sleep(50 * time.Millisecond)

	stream, err := clientConn.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	nw, err := stream.Write(sendData)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if nw != dataSize {
		t.Fatalf("Write: wrote %d, want %d", nw, dataSize)
	}

	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	select {
	case serverErr := <-errCh:
		t.Fatalf("server error: %v", serverErr)
	case received := <-recvCh:
		if len(received) != dataSize {
			t.Fatalf("received %d bytes, want %d", len(received), dataSize)
		}
		if !bytes.Equal(received, sendData) {
			t.Fatalf("received data does not match sent data")
		}
		t.Logf("successfully sent and verified %d bytes", dataSize)
	case <-ctx.Done():
		t.Fatalf("test timed out")
	}
}
