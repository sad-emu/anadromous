//go:build linux

package anadromous

import (
	"context"
	"testing"
	"time"
)

func TestWireAccounting(t *testing.T) {
	tests := []struct {
		name    string
		cfg     WireAccounting
		payload int
		want    int64
	}{
		{name: "UDP payload", payload: 8500, want: 8500},
		{name: "IPv4 packet", cfg: WireAccounting{PerDatagramOverhead: 28}, payload: 8500, want: 8528},
		{name: "physical jumbo Ethernet", cfg: WireAccounting{PerDatagramOverhead: 66, MinimumDatagramSize: 84}, payload: 8500, want: 8566},
		{name: "physical minimum Ethernet", cfg: WireAccounting{PerDatagramOverhead: 66, MinimumDatagramSize: 84}, payload: frameHeaderSize, want: 84},
		{name: "negative values clamp", cfg: WireAccounting{PerDatagramOverhead: -10, MinimumDatagramSize: -20}, payload: 100, want: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Cost(tt.payload); got != tt.want {
				t.Fatalf("Cost(%d) = %d, want %d", tt.payload, got, tt.want)
			}
		})
	}
}

func TestBatchAccountingCountsEveryGSOSegment(t *testing.T) {
	p := NewWirePacer(WirePacerConfig{
		RateBytesPerSecond: 1_250_000_000,
		Accounting: WireAccounting{
			PerDatagramOverhead: 66,
			MinimumDatagramSize: 84,
		},
	})
	c := &Connection{wirePacer: p, sendPaceClass: paceBulk}
	for i := 0; i < 7; i++ {
		// Seven datagrams may become one GSO super-packet, but the carrier
		// charges seven packets and therefore seven copies of the overhead.
		c.accountDatagramLocked(8500, paceBulk)
	}
	if c.sendDatagrams != 7 || c.sendUDPBytes != 7*8500 || c.sendWireBytes != 7*8566 {
		t.Fatalf("batch accounting = datagrams %d, UDP %d, wire %d; want 7/%d/%d",
			c.sendDatagrams, c.sendUDPBytes, c.sendWireBytes, 7*8500, 7*8566)
	}
}

func TestMixedTrafficCannotPromoteBulkBatchToCritical(t *testing.T) {
	p := NewWirePacer(WirePacerConfig{RateBytesPerSecond: 1_000_000})
	c := &Connection{wirePacer: p, sendPaceClass: paceBulk}
	c.accountDatagramLocked(8_500, paceBulk)
	c.accountDatagramLocked(frameHeaderSize, paceCritical)
	if c.sendPaceClass != paceBulk {
		t.Fatalf("mixed DATA+ACK batch class = %v, want bulk", c.sendPaceClass)
	}

	c.resetBatchAccountingLocked()
	c.accountDatagramLocked(frameHeaderSize, paceCritical)
	c.accountDatagramLocked(8_500, paceRecovery)
	if c.sendPaceClass != paceRecovery {
		t.Fatalf("mixed ACK+recovery batch class = %v, want recovery", c.sendPaceClass)
	}
}

func TestWirePacerOneBudgetAcrossBatches(t *testing.T) {
	const (
		rate  = int64(100_000) // bytes/s
		burst = int64(1_000)
	)
	p := NewWirePacer(WirePacerConfig{RateBytesPerSecond: rate, BurstBytes: burst})
	if !p.waitBatch(burst, burst, 1, paceBulk, nil) {
		t.Fatal("initial batch was not granted")
	}
	started := time.Now()
	if !p.waitBatch(burst, burst, 1, paceBulk, nil) {
		t.Fatal("second batch was not granted")
	}
	elapsed := time.Since(started)
	// Both calls visit the same token state. An accidentally per-connection
	// bucket would grant the second full burst immediately.
	if elapsed < 5*time.Millisecond {
		t.Fatalf("shared second batch waited only %v, want roughly 10ms", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("shared second batch waited unexpectedly long: %v", elapsed)
	}
	stats := p.Stats()
	if stats.WireBytes != 2*uint64(burst) || stats.UDPBytes != 2*uint64(burst) || stats.Datagrams != 2 || stats.Batches != 2 {
		t.Fatalf("stats = %+v, want two 1000-byte batches/datagrams", stats)
	}
}

func TestCriticalDebtIsBoundedAndRepaid(t *testing.T) {
	p := NewWirePacer(WirePacerConfig{RateBytesPerSecond: 100_000, BurstBytes: 100})
	if !p.waitBatch(1_000, frameHeaderSize, 1, paceCritical, nil) {
		t.Fatal("initial critical batch was not granted")
	}
	started := time.Now()
	if !p.waitBatch(1_000, frameHeaderSize, 1, paceCritical, nil) {
		t.Fatal("second critical batch was not granted")
	}
	if elapsed := time.Since(started); elapsed < 4*time.Millisecond {
		t.Fatalf("second critical batch escaped bounded debt after only %v", elapsed)
	} else if elapsed > 500*time.Millisecond {
		t.Fatalf("second critical batch waited unexpectedly long: %v", elapsed)
	}
	stats := p.Stats()
	if stats.WireBytes != 2_000 || stats.UDPBytes != 2*frameHeaderSize || stats.CriticalBatches != 2 {
		t.Fatalf("critical batch stats = %+v", stats)
	}
}

func TestCriticalReserveUsesPostChargeBound(t *testing.T) {
	const burst = int64(100)
	for _, batch := range []int64{1, 10, 100, 150, 200} {
		p := NewWirePacer(WirePacerConfig{RateBytesPerSecond: 1_000_000, BurstBytes: burst})
		needed := batch - burst
		if needed > burst {
			needed = burst
		}
		p.mu.Lock()
		p.tokens = needed
		p.last = time.Now()
		p.mu.Unlock()
		if !p.waitBatch(batch, batch, 1, paceCritical, nil) {
			t.Fatalf("critical batch %d was not granted", batch)
		}
		p.mu.Lock()
		remaining := p.tokens
		p.mu.Unlock()
		if remaining < -burst {
			t.Fatalf("critical batch %d left tokens %d below reserve floor %d", batch, remaining, -burst)
		}
	}
}

func TestWithPacingRateMaterializesPrivateConnectionPacers(t *testing.T) {
	cfg := defaultConfig()
	WithPacingRate(1_000_000)(&cfg)
	WithPacingAccounting(WireAccounting{PerDatagramOverhead: 66, MinimumDatagramSize: 84})(&cfg)
	WithPacingBurstBytes(4096)(&cfg)
	first := cfg
	first.ensureWirePacer()
	second := cfg
	second.ensureWirePacer()
	if first.wirePacer == nil || second.wirePacer == nil {
		t.Fatal("WithPacingRate did not materialize a pacer")
	}
	if first.wirePacer == second.wirePacer {
		t.Fatal("legacy WithPacingRate unexpectedly shared connection token state")
	}
	if first.wirePacer.BurstBytes() != 4096 || first.wirePacer.Accounting().PerDatagramOverhead != 66 {
		t.Fatalf("materialized pacer config = burst %d accounting %+v", first.wirePacer.BurstBytes(), first.wirePacer.Accounting())
	}
}

func TestPacingOptionsUseLastRateSource(t *testing.T) {
	p := NewWirePacer(WirePacerConfig{RateBytesPerSecond: 2_000_000})
	cfg := defaultConfig()
	WithPacingRate(1_000_000)(&cfg)
	WithWirePacer(nil)(&cfg)
	cfg.ensureWirePacer()
	if cfg.wirePacer != nil || cfg.paceRate != 0 {
		t.Fatal("WithWirePacer(nil) did not disable a preceding legacy rate")
	}

	WithWirePacer(p)(&cfg)
	WithPacingRate(3_000_000)(&cfg)
	cfg.ensureWirePacer()
	if cfg.wirePacer == nil || cfg.wirePacer == p || cfg.wirePacer.RateBytesPerSecond() != 3_000_000 {
		t.Fatal("later WithPacingRate did not replace the explicit shared pacer")
	}
}

func TestWirePacerPointerSharedByDialPoolAndListenerConnections(t *testing.T) {
	serverPacer := NewWirePacer(WirePacerConfig{RateBytesPerSecond: 1_250_000_000})
	ln, err := Listen("127.0.0.1:0", WithWirePacer(serverPacer), WithFEC(0))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	clientPacer := NewWirePacer(WirePacerConfig{RateBytesPerSecond: 1_250_000_000})
	dial := func() *Connection {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := Dial(ctx, ln.Addr().String(), WithWirePacer(clientPacer), WithFEC(0))
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	c1 := dial()
	s1, err := ln.Accept(context.Background())
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	c2 := dial()
	s2, err := ln.Accept(context.Background())
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}

	if c1.wirePacer != clientPacer || c2.wirePacer != clientPacer {
		t.Fatal("dial pool did not retain one caller-owned wire pacer")
	}
	if s1.wirePacer != serverPacer || s2.wirePacer != serverPacer {
		t.Fatal("listener connections did not retain one endpoint wire pacer")
	}
}

func BenchmarkWirePacer10G64K(b *testing.B) {
	const batchBytes = int64(64 << 10)
	p := NewWirePacer(WirePacerConfig{RateBytesPerSecond: 1_250_000_000})
	b.SetBytes(batchBytes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !p.waitBatch(batchBytes, batchBytes, 8, paceBulk, nil) {
			b.Fatal("pacer grant cancelled")
		}
	}
}
