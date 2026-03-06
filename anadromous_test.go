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

// TestInOrderPackets verifies that packets arriving in order are handled correctly
func TestInOrderPackets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	t.Logf("listener on %s", addr)

	// Multiple messages to send in order
	messages := []string{"hello", "world", "foo", "bar", "baz"}
	errCh := make(chan error, 1)

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

		// Read all data until EOF
		allData := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				allData = append(allData, buf[:n]...)
				t.Logf("server read %d bytes", n)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				errCh <- fmt.Errorf("server Read: %w", rerr)
				return
			}
		}

		// Parse and verify received messages are in order
		received := string(allData)
		t.Logf("server received all data (%d bytes): %q", len(allData), received)

		// Build expected data
		expected := ""
		for _, msg := range messages {
			expected += msg + "\n"
		}

		if received != expected {
			errCh <- fmt.Errorf("received data does not match expected.\nGot: %q\nWant: %q", received, expected)
			return
		}

		// Verify each message is in order
		lines := bytes.Split([]byte(received), []byte("\n"))
		for i, expectedMsg := range messages {
			if i < len(lines) && string(lines[i]) == expectedMsg {
				t.Logf("server verified message %d in order: %q", i, expectedMsg)
			}
		}
		errCh <- nil
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
	defer stream.Close()

	// Send all messages in order with newline delimiter
	for i, msg := range messages {
		msgWithDelim := msg + "\n"
		nw, err := stream.Write([]byte(msgWithDelim))
		if err != nil {
			t.Fatalf("Write message %d: %v", i, err)
		}
		if nw != len(msgWithDelim) {
			t.Fatalf("Write message %d: wrote %d, want %d", i, nw, len(msgWithDelim))
		}
		t.Logf("client sent message %d: %q (wrote %d bytes)", i, msg, nw)
		time.Sleep(5 * time.Millisecond) // Small delay to allow messages to be sent separately
	}

	// Close write side to signal EOF to server
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	select {
	case serverErr := <-errCh:
		if serverErr != nil {
			t.Fatalf("server error: %v", serverErr)
		}
		t.Logf("successfully sent and received %d messages in order", len(messages))
	case <-ctx.Done():
		t.Fatalf("test timed out")
	}
}
