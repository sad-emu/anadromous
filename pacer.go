//go:build linux

package anadromous

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/tredeske/u/unet"
	"golang.org/x/sys/unix"
)

const defaultPacingBurst = 2 * time.Millisecond

// WireAccounting describes how a carrier accounts one UDP datagram. The
// protocol passes the UDP payload length (the encoded Anadromous frame) and
// the accounting adds bytes outside it. MinimumDatagramSize models media that
// pad short packets to a minimum charged size.
//
// Common examples are:
//
//   - UDP payload policer:                 overhead 0,  minimum 0
//   - IPv4 packet bytes:                   overhead 28, minimum 0
//   - physical IPv4 Ethernet incl. IFG:    overhead 66, minimum 84
//
// VLAN/QinQ or a carrier-specific service-frame definition can be represented
// by increasing PerDatagramOverhead. Negative values are treated as zero.
type WireAccounting struct {
	PerDatagramOverhead int64
	MinimumDatagramSize int64
}

// Cost returns the carrier-counted bytes for one UDP datagram.
func (a WireAccounting) Cost(udpPayloadBytes int) int64 {
	overhead := a.PerDatagramOverhead
	if overhead < 0 {
		overhead = 0
	}
	minimum := a.MinimumDatagramSize
	if minimum < 0 {
		minimum = 0
	}
	cost := int64(udpPayloadBytes) + overhead
	if cost < minimum {
		cost = minimum
	}
	return cost
}

// WirePacerConfig configures a WirePacer. RateBytesPerSecond is the aggregate
// egress budget shared by every Connection using the pacer. BurstBytes zero
// selects two milliseconds of rate; an individual batch/datagram may exceed
// the burst because it cannot be split after it has been encoded.
type WirePacerConfig struct {
	RateBytesPerSecond int64
	BurstBytes         int64
	Accounting         WireAccounting
}

// WirePacerStats is a monotonic snapshot of successfully granted batches.
// WireBytes includes Accounting overhead; UDPBytes is encoded protocol bytes.
type WirePacerStats struct {
	WireBytes       uint64
	UDPBytes        uint64
	Datagrams       uint64
	Batches         uint64
	CriticalBatches uint64
	RecoveryBatches uint64
	BulkBatches     uint64
	WaitTime        time.Duration
}

// WirePacer is a concurrency-safe aggregate token bucket. A single instance
// may be shared by every UDP Connection that consumes one provisioned egress
// circuit. Connections account frames without taking this mutex; only a
// completed send batch visits the bucket, removing per-datagram clock reads,
// locks and sleeps from the bulk path.
type WirePacer struct {
	rate       int64
	burstBytes int64
	accounting WireAccounting

	mu     sync.Mutex
	tokens int64
	last   time.Time

	recoveryWaiters atomic.Int64
	criticalWaiters atomic.Int64

	statWireBytes       atomic.Uint64
	statUDPBytes        atomic.Uint64
	statDatagrams       atomic.Uint64
	statBatches         atomic.Uint64
	statCriticalBatches atomic.Uint64
	statRecoveryBatches atomic.Uint64
	statBulkBatches     atomic.Uint64
	statWaitNanos       atomic.Int64
}

// NewWirePacer constructs a reusable aggregate wire pacer. A non-positive
// rate returns nil, which is accepted by WithWirePacer as pacing disabled.
func NewWirePacer(cfg WirePacerConfig) *WirePacer {
	if cfg.RateBytesPerSecond <= 0 {
		return nil
	}
	burst := cfg.BurstBytes
	if burst <= 0 {
		burst = cfg.RateBytesPerSecond * int64(defaultPacingBurst) / int64(time.Second)
	}
	if burst < 1 {
		burst = 1
	}
	if cfg.Accounting.PerDatagramOverhead < 0 {
		cfg.Accounting.PerDatagramOverhead = 0
	}
	if cfg.Accounting.MinimumDatagramSize < 0 {
		cfg.Accounting.MinimumDatagramSize = 0
	}
	now := time.Now()
	return &WirePacer{
		rate:       cfg.RateBytesPerSecond,
		burstBytes: burst,
		accounting: cfg.Accounting,
		tokens:     burst,
		last:       now,
	}
}

// RateBytesPerSecond returns the aggregate configured rate.
func (p *WirePacer) RateBytesPerSecond() int64 {
	if p == nil {
		return 0
	}
	return p.rate
}

// BurstBytes returns the maximum ordinary batch quantum.
func (p *WirePacer) BurstBytes() int64 {
	if p == nil {
		return 0
	}
	return p.burstBytes
}

// Accounting returns the immutable carrier accounting configuration.
func (p *WirePacer) Accounting() WireAccounting {
	if p == nil {
		return WireAccounting{}
	}
	return p.accounting
}

// Stats returns a lock-free, internally consistent-enough monitoring snapshot.
// Individual fields may advance between loads, but all are monotonic.
func (p *WirePacer) Stats() WirePacerStats {
	if p == nil {
		return WirePacerStats{}
	}
	return WirePacerStats{
		WireBytes:       p.statWireBytes.Load(),
		UDPBytes:        p.statUDPBytes.Load(),
		Datagrams:       p.statDatagrams.Load(),
		Batches:         p.statBatches.Load(),
		CriticalBatches: p.statCriticalBatches.Load(),
		RecoveryBatches: p.statRecoveryBatches.Load(),
		BulkBatches:     p.statBulkBatches.Load(),
		WaitTime:        time.Duration(p.statWaitNanos.Load()),
	}
}

type paceClass uint8

const (
	paceCritical paceClass = iota
	paceRecovery
	paceBulk
)

// refillLocked adds tokens earned since the preceding visit. Floating point
// is used only for the elapsed-time conversion; the result is clamped before
// conversion, avoiding integer overflow after a long idle period.
func (p *WirePacer) refillLocked(now time.Time) {
	if !now.After(p.last) {
		return
	}
	space := p.burstBytes - p.tokens
	if space <= 0 {
		p.tokens = p.burstBytes
		p.last = now
		return
	}
	addedFloat := float64(now.Sub(p.last)) * float64(p.rate) / float64(time.Second)
	if addedFloat >= float64(space) {
		p.tokens = p.burstBytes
		p.last = now
	} else if addedFloat >= 1 {
		p.tokens += int64(addedFloat)
		p.last = now
	}
}

// waitBatch grants one already-built batch. Critical protocol traffic may use
// one burst of bounded debt so ACK and flow-control progress are not normally
// delayed behind bulk output; once that reserve is exhausted, later critical
// traffic waits and repays it. Critical and recovery waiters take precedence
// over new bulk batches across every Connection sharing this pacer.
func (p *WirePacer) waitBatch(wireBytes, udpBytes int64, datagrams int, class paceClass, done <-chan struct{}) bool {
	if p == nil || p.rate <= 0 || wireBytes <= 0 {
		return true
	}

	if class == paceCritical {
		p.criticalWaiters.Add(1)
		defer p.criticalWaiters.Add(-1)
	} else if class == paceRecovery {
		p.recoveryWaiters.Add(1)
		defer p.recoveryWaiters.Add(-1)
	}
	waitStarted := time.Now()
	for {
		if (class == paceBulk && (p.criticalWaiters.Load() > 0 || p.recoveryWaiters.Load() > 0)) ||
			(class == paceRecovery && p.criticalWaiters.Load() > 0) {
			if !waitPacerDelay(100*time.Microsecond, done) {
				return false
			}
			continue
		}

		p.mu.Lock()
		now := time.Now()
		p.refillLocked(now)
		// Critical traffic may use one burst of reserve (post-charge tokens down
		// to -burst) before it waits for repayment. A batch larger than two
		// bursts is indivisible and may cross that floor once it accumulates a
		// full bucket; the next batch then repays the complete excess.
		needed := wireBytes
		if class == paceCritical {
			needed -= p.burstBytes
		}
		if needed > p.burstBytes {
			needed = p.burstBytes
		}
		if p.tokens >= needed {
			p.tokens -= wireBytes
			p.mu.Unlock()
			p.recordGrant(wireBytes, udpBytes, datagrams, class, time.Since(waitStarted))
			return true
		}
		deficit := needed - p.tokens
		p.mu.Unlock()

		wait := time.Duration(float64(deficit) / float64(p.rate) * float64(time.Second))
		if wait < time.Microsecond {
			wait = time.Microsecond
		}
		if !waitPacerDelay(wait, done) {
			return false
		}
	}
}

func waitPacerDelay(delay time.Duration, done <-chan struct{}) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	if done == nil {
		<-timer.C
		return true
	}
	select {
	case <-timer.C:
		return true
	case <-done:
		return false
	}
}

func (p *WirePacer) recordGrant(wireBytes, udpBytes int64, datagrams int, class paceClass, waited time.Duration) {
	p.statWireBytes.Add(uint64(wireBytes))
	p.statUDPBytes.Add(uint64(udpBytes))
	p.statDatagrams.Add(uint64(datagrams))
	p.statBatches.Add(1)
	p.statWaitNanos.Add(int64(waited))
	switch class {
	case paceCritical:
		p.statCriticalBatches.Add(1)
	case paceRecovery:
		p.statRecoveryBatches.Add(1)
	default:
		p.statBulkBatches.Add(1)
	}
}

// setKernelPacingRate installs SO_MAX_PACING_RATE as a best-effort per-socket
// safety cap. sch_fq-capable interfaces can use it to spread a submitted GSO /
// sendmmsg batch in the kernel. Aggregate enforcement still comes from the
// shared WirePacer because this socket option has no cross-socket scope.
func setKernelPacingRate(p *WirePacer) unet.SockOpt {
	return func(s *unet.Socket) error {
		if p == nil || p.rate <= 0 {
			return nil
		}
		fd, ok := s.Fd.Get()
		if !ok {
			return ErrClosed
		}
		rate := p.rate
		const maxKernelPacingRate = int64(^uint32(0))
		if rate > maxKernelPacingRate {
			rate = maxKernelPacingRate
		}
		// Unsupported kernels/qdiscs retain the userspace batch pacer. Failure
		// here must therefore not prevent a connection from being created.
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MAX_PACING_RATE, int(rate))
		return nil
	}
}
