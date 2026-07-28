//go:build linux

package anadromous

// uring.go is a minimal io_uring wrapper for the batched-async send path.
//
// The sendmmsg flush is synchronous: the caller's thread sits in the kernel
// while every datagram in the batch is segmented (GSO) and delivered, so at
// high rates the Write path spends most of its time inside one syscall. The
// io_uring path instead submits the batch as IOSQE_ASYNC SENDMSG operations
// — forcing them onto the kernel's io-wq worker threads — and returns
// without waiting, so the network-stack work runs concurrently with the
// application producing the next batch. Completions are reaped on the NEXT
// flush, which pipelines batches one deep: batch N is in flight while batch
// N+1 is being built (see Connection.flushSendUringLocked for the
// double-buffering and buffer-lifetime rules this requires).
//
// Only submission-side plumbing is implemented — enough for SENDMSG with
// completions counted per generation — not a general io_uring library.
// Kernels without io_uring or without IORING_FEAT_SINGLE_MMAP (pre-5.4)
// fall back to sendmmsg transparently.

import (
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/tredeske/u/unet"
)

const (
	sysIOUringSetup = 425
	sysIOUringEnter = 426

	ioringOpSendmsg = 9

	ioringEnterGetevents = 1 << 0

	ioringOffSqRing = 0
	ioringOffSqes   = 0x10000000

	ioringFeatSingleMmap = 1 << 0

	// IOSQE_ASYNC: skip the inline non-blocking attempt and hand the op
	// straight to an io-wq worker. Without it, UDP sendmsg virtually always
	// completes inline during io_uring_enter — same blocking behavior as
	// sendmmsg with extra overhead.
	iosqeAsync = 1 << 4
)

type ioSqringOffsets struct {
	head, tail, ringMask, ringEntries uint32
	flags, dropped, array, resv1      uint32
	userAddr                          uint64
}

type ioCqringOffsets struct {
	head, tail, ringMask, ringEntries uint32
	overflow, cqes, flags, resv1      uint32
	userAddr                          uint64
}

type ioUringParams struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCpu  uint32
	sqThreadIdle uint32
	features     uint32
	wqFd         uint32
	resv         [3]uint32
	sqOff        ioSqringOffsets
	cqOff        ioCqringOffsets
}

// ioUringSqe matches struct io_uring_sqe (64 bytes, non-SQE128 layout).
type ioUringSqe struct {
	opcode      uint8
	flags       uint8
	ioprio      uint16
	fd          int32
	off         uint64
	addr        uint64 // for SENDMSG: pointer to the struct msghdr
	len         uint32 // for SENDMSG: 1
	msgFlags    uint32
	userData    uint64
	bufIndex    uint16
	personality uint16
	spliceFdIn  int32
	pad2        [2]uint64
}

// ioUringCqe matches struct io_uring_cqe (16 bytes, non-CQE32 layout).
type ioUringCqe struct {
	userData uint64
	res      int32
	flags    uint32
}

// sendRing owns one io_uring instance dedicated to a Connection's sends.
// All access is serialized by the Connection's sendMu.
type sendRing struct {
	fd     int
	ring   []byte // shared SQ/CQ ring mapping (FEAT_SINGLE_MMAP)
	sqeMem []byte // SQE array mapping

	sqHead, sqTail *uint32
	sqMask         uint32
	sqArray        []uint32
	sqes           []ioUringSqe

	cqHead, cqTail *uint32
	cqMask         uint32
	cqes           []ioUringCqe

	// outstanding counts un-reaped submissions per user_data generation
	// (the Connection's two send-slot sets).
	outstanding [2]int
	// firstErr holds the first failed completion's errno until collected.
	firstErr syscall.Errno
}

// newSendRing sets up an io_uring sized for at least sqSize concurrent
// submissions. Returns nil (not an error) when the kernel can't provide one
// — callers fall back to sendmmsg.
func newSendRing(sqSize int) *sendRing {
	var p ioUringParams
	entries := uintptr(sqSize)
	rfd, _, errno := syscall.Syscall(sysIOUringSetup, entries, uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil
	}
	r := &sendRing{fd: int(rfd)}
	if p.features&ioringFeatSingleMmap == 0 {
		syscall.Close(r.fd)
		return nil
	}

	sqRingSz := int(p.sqOff.array + p.sqEntries*4)
	cqRingSz := int(p.cqOff.cqes + p.cqEntries*16)
	ringSz := sqRingSz
	if cqRingSz > ringSz {
		ringSz = cqRingSz
	}
	var err error
	r.ring, err = syscall.Mmap(r.fd, ioringOffSqRing, ringSz,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		syscall.Close(r.fd)
		return nil
	}
	r.sqeMem, err = syscall.Mmap(r.fd, ioringOffSqes, int(p.sqEntries)*int(unsafe.Sizeof(ioUringSqe{})),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		syscall.Munmap(r.ring)
		syscall.Close(r.fd)
		return nil
	}

	at32 := func(off uint32) *uint32 { return (*uint32)(unsafe.Pointer(&r.ring[off])) }
	r.sqHead = at32(p.sqOff.head)
	r.sqTail = at32(p.sqOff.tail)
	r.sqMask = *at32(p.sqOff.ringMask)
	r.sqArray = unsafe.Slice(at32(p.sqOff.array), p.sqEntries)
	r.sqes = unsafe.Slice((*ioUringSqe)(unsafe.Pointer(&r.sqeMem[0])), p.sqEntries)
	r.cqHead = at32(p.cqOff.head)
	r.cqTail = at32(p.cqOff.tail)
	r.cqMask = *at32(p.cqOff.ringMask)
	r.cqes = unsafe.Slice((*ioUringCqe)(unsafe.Pointer(&r.ring[p.cqOff.cqes])), p.cqEntries)
	return r
}

func (r *sendRing) enter(toSubmit, minComplete, flags int) (consumed int, errno syscall.Errno) {
	n, _, errno := syscall.Syscall6(sysIOUringEnter, uintptr(r.fd),
		uintptr(toSubmit), uintptr(minComplete), uintptr(flags), 0, 0)
	return int(n), errno
}

// submitSendmsg queues one SENDMSG per msghdr and submits them in a single
// io_uring_enter. gen tags the completions (see outstanding); sqeFlags is 0
// or iosqeAsync — inline (0) submissions execute during the enter itself
// (UDP sendmsg completes non-blocking), giving sendmmsg-like latency for
// small control flushes, while iosqeAsync buys bulk batches the io-wq
// pipelining at the cost of a worker wakeup. The msghdrs, their iovecs, and
// everything the iovecs point at must stay untouched until the completions
// are reaped.
func (r *sendRing) submitSendmsg(sockFd int, hdrs []unet.MMsghdr, gen int, sqeFlags uint8) syscall.Errno {
	for len(hdrs) > 0 {
		tail := *r.sqTail
		head := atomic.LoadUint32(r.sqHead)
		free := len(r.sqes) - int(tail-head)
		n := len(hdrs)
		if n > free {
			n = free
		}
		if n == 0 {
			// SQ full — can't happen with our sizing (the ring holds two
			// full batches), but wait a completion out rather than spin.
			if _, errno := r.enter(0, 1, ioringEnterGetevents); errno != 0 && errno != syscall.EINTR {
				return errno
			}
			r.reap()
			continue
		}
		for i := 0; i < n; i++ {
			idx := (tail + uint32(i)) & r.sqMask
			r.sqes[idx] = ioUringSqe{
				opcode:   ioringOpSendmsg,
				flags:    sqeFlags,
				fd:       int32(sockFd),
				addr:     uint64(uintptr(unsafe.Pointer(&hdrs[i].Msghdr))),
				len:      1,
				userData: uint64(gen),
			}
			r.sqArray[idx] = idx
		}
		atomic.StoreUint32(r.sqTail, tail+uint32(n))
		for toSubmit := n; toSubmit > 0; {
			consumed, errno := r.enter(toSubmit, 0, 0)
			if errno != 0 {
				if errno == syscall.EINTR {
					continue
				}
				return errno
			}
			toSubmit -= consumed
		}
		r.outstanding[gen] += n
		hdrs = hdrs[n:]
	}
	return 0
}

// reap consumes every available CQE, decrementing the per-generation
// outstanding counts and recording the first error result.
func (r *sendRing) reap() {
	head := *r.cqHead
	tail := atomic.LoadUint32(r.cqTail)
	for ; head != tail; head++ {
		cqe := &r.cqes[head&r.cqMask]
		if g := cqe.userData; g < 2 {
			r.outstanding[g]--
		}
		if cqe.res < 0 && r.firstErr == 0 {
			r.firstErr = syscall.Errno(-cqe.res)
		}
	}
	atomic.StoreUint32(r.cqHead, head)
}

// waitGen blocks until every submission tagged gen has completed.
func (r *sendRing) waitGen(gen int) syscall.Errno {
	for {
		r.reap()
		if r.outstanding[gen] <= 0 {
			return 0
		}
		if _, errno := r.enter(0, 1, ioringEnterGetevents); errno != 0 && errno != syscall.EINTR {
			return errno
		}
	}
}

// collectErr returns and clears the first completion error seen so far.
func (r *sendRing) collectErr() syscall.Errno {
	e := r.firstErr
	r.firstErr = 0
	return e
}

// close drains all outstanding completions, then releases the ring.
func (r *sendRing) close() {
	r.waitGen(0)
	r.waitGen(1)
	syscall.Munmap(r.sqeMem)
	syscall.Munmap(r.ring)
	syscall.Close(r.fd)
}
