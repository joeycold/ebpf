package perf

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/cilium/ebpf/internal/unix"
	linux "golang.org/x/sys/unix"
)

// perfEventRing is a page of metadata followed by
// a variable number of pages which form a ring buffer.
type perfEventRing struct {
	fd   int
	cpu  int
	mmap []byte
	ringReader
}

func newPerfEventRing(cpu, perCPUBuffer, watermark int, overwritable bool, eopts ExtraPerfOptions) (*perfEventRing, error) {
	if watermark >= perCPUBuffer {
		return nil, errors.New("watermark must be smaller than perCPUBuffer")
	}

	fd, err := createPerfEvent(cpu, watermark, overwritable, eopts)
	if err != nil {
		return nil, err
	}

	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}

	protections := unix.PROT_READ
	if !overwritable {
		protections |= unix.PROT_WRITE
	}

	mmap, err := unix.Mmap(fd, 0, perfBufferSize(perCPUBuffer), protections, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("can't mmap: %v", err)
	}

	// This relies on the fact that we allocate an extra metadata page,
	// and that the struct is smaller than an OS page.
	// This use of unsafe.Pointer isn't explicitly sanctioned by the
	// documentation, since a byte is smaller than sampledPerfEvent.
	meta := (*unix.PerfEventMmapPage)(unsafe.Pointer(&mmap[0]))

	var reader ringReader
	if overwritable {
		reader = newReverseReader(meta, mmap[meta.Data_offset:meta.Data_offset+meta.Data_size])
	} else {
		reader = newForwardReader(meta, mmap[meta.Data_offset:meta.Data_offset+meta.Data_size])
	}

	ring := &perfEventRing{
		fd:         fd,
		cpu:        cpu,
		mmap:       mmap,
		ringReader: reader,
	}
	runtime.SetFinalizer(ring, (*perfEventRing).Close)

	return ring, nil
}

// perfBufferSize returns a valid mmap buffer size for use with perf_event_open (1+2^n pages)
func perfBufferSize(perCPUBuffer int) int {
	pageSize := os.Getpagesize()

	// Smallest whole number of pages
	nPages := (perCPUBuffer + pageSize - 1) / pageSize

	// Round up to nearest power of two number of pages
	nPages = int(math.Pow(2, math.Ceil(math.Log2(float64(nPages)))))

	// Add one for metadata
	nPages += 1

	return nPages * pageSize
}

func (ring *perfEventRing) Close() {
	runtime.SetFinalizer(ring, nil)

	_ = unix.Close(ring.fd)
	_ = unix.Munmap(ring.mmap)

	ring.fd = -1
	ring.mmap = nil
}

const (
	HW_BREAKPOINT_LEN_1 = 1
	HW_BREAKPOINT_LEN_2 = 2
	HW_BREAKPOINT_LEN_3 = 3
	HW_BREAKPOINT_LEN_4 = 4
	HW_BREAKPOINT_LEN_5 = 5
	HW_BREAKPOINT_LEN_6 = 6
	HW_BREAKPOINT_LEN_7 = 7
	HW_BREAKPOINT_LEN_8 = 8
)

func createPerfEvent(cpu, watermark int, overwritable bool, eopts ExtraPerfOptions) (int, error) {
	if watermark == 0 {
		watermark = 1
	}

	bits := linux.PerfBitWatermark
	if overwritable {
		bits |= linux.PerfBitWriteBackward
	}

	var attr linux.PerfEventAttr

	watch_pid := -1
	if eopts.BrkAddr != 0 {
		watch_pid = eopts.BrkPid
		attr = unix.PerfEventAttr{
			Type:        linux.PERF_TYPE_BREAKPOINT,
			Config:      linux.PERF_COUNT_SW_CPU_CLOCK,
			Bits:        uint64(bits),
			Sample_type: linux.PERF_SAMPLE_ADDR | linux.PERF_SAMPLE_TID,
			// Generate a notification every 1 event; we care about every event
			Sample:  1,
			Wakeup:  1,
			Bp_type: eopts.BrkType,
			Ext1:    eopts.BrkAddr,
			Ext2:    eopts.BrkLen,
			// Ext2:    HW_BREAKPOINT_LEN_4,
		}
	} else {
		attr = unix.PerfEventAttr{
			Type:        linux.PERF_TYPE_SOFTWARE,
			Config:      linux.PERF_COUNT_SW_BPF_OUTPUT,
			Bits:        uint64(bits),
			Sample_type: linux.PERF_SAMPLE_RAW,
			Wakeup:      uint32(watermark),
		}
	}

	if eopts.UnwindStack {
		attr.Sample_type |= linux.PERF_SAMPLE_STACK_USER | linux.PERF_SAMPLE_REGS_USER
		attr.Sample_regs_user = eopts.Sample_regs_user
		attr.Sample_stack_user = eopts.Sample_stack_user
	} else if eopts.ShowRegs {
		// 只获取寄存器信息
		attr.Sample_type |= linux.PERF_SAMPLE_REGS_USER
		attr.Sample_regs_user = eopts.Sample_regs_user
	}
	if eopts.PerfMmap {
		attr.Bits |= linux.PerfBitMmap
		attr.Bits |= linux.PerfBitComm
		// // mmap_data 标志位用来获取不可执行的 mmap 相关数据 比如获取 vdex 之类的信息
		// // 但是应该只需要一次 simpleperf 说明如下
		// // To profile java code, need to dump maps containing vdex files, which are not executable.
		attr.Bits |= linux.PerfBitMmapData
		// 考虑到要给 5.10+ 内核用户使用 那么这里之间设置 mmap2 的标志位即可
		// 实际上 mmap2 标志位生效的前提是 mmap 也设置了
		attr.Bits |= linux.PerfBitMmap2
	}

	attr.Size = uint32(unsafe.Sizeof(attr))
	fd, err := unix.PerfEventOpen(&attr, watch_pid, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("can't create perf event: %w", err)
	}
	return fd, nil
}

type ringReader interface {
	loadHead()
	size() int
	writeTail()
	Read(p []byte) (int, error)
}

type forwardReader struct {
	meta       *unix.PerfEventMmapPage
	head, tail uint64
	mask       uint64
	ring       []byte
}

func newForwardReader(meta *unix.PerfEventMmapPage, ring []byte) *forwardReader {
	return &forwardReader{
		meta: meta,
		head: atomic.LoadUint64(&meta.Data_head),
		tail: atomic.LoadUint64(&meta.Data_tail),
		// cap is always a power of two
		mask: uint64(cap(ring) - 1),
		ring: ring,
	}
}

func (rr *forwardReader) loadHead() {
	rr.head = atomic.LoadUint64(&rr.meta.Data_head)
}

func (rr *forwardReader) size() int {
	return len(rr.ring)
}

func (rr *forwardReader) writeTail() {
	// Commit the new tail. This lets the kernel know that
	// the ring buffer has been consumed.
	atomic.StoreUint64(&rr.meta.Data_tail, rr.tail)
}

func (rr *forwardReader) Read(p []byte) (int, error) {
	start := int(rr.tail & rr.mask)

	n := len(p)
	// Truncate if the read wraps in the ring buffer
	if remainder := cap(rr.ring) - start; n > remainder {
		n = remainder
	}

	// Truncate if there isn't enough data
	if remainder := int(rr.head - rr.tail); n > remainder {
		n = remainder
	}

	copy(p, rr.ring[start:start+n])
	rr.tail += uint64(n)

	if rr.tail == rr.head {
		return n, io.EOF
	}

	return n, nil
}

type reverseReader struct {
	meta *unix.PerfEventMmapPage
	// head is the position where the kernel last wrote data.
	head uint64
	// read is the position we read the next data from. Updated as reads are made.
	read uint64
	// tail is the end of the ring buffer. No reads must be made past it.
	tail uint64
	mask uint64
	ring []byte
}

func newReverseReader(meta *unix.PerfEventMmapPage, ring []byte) *reverseReader {
	rr := &reverseReader{
		meta: meta,
		mask: uint64(cap(ring) - 1),
		ring: ring,
	}
	rr.loadHead()
	return rr
}

func (rr *reverseReader) loadHead() {
	// The diagram below represents an overwritable perf ring buffer:
	//
	//    head     read                            tail
	//     |        |                               |
	//     V        V                               V
	// +---+--------+------------+---------+--------+
	// |   |H-D....D|H-C........C|H-B.....B|H-A....A|
	// +---+--------+------------+---------+--------+
	// <--Write from right to left
	//                     Read from left to right-->
	// (H means header)
	//
	// The buffer is read left to right beginning from head to tail.
	// [head, read) is the read portion of the buffer, [read, tail) the unread one.
	// read is adjusted as we progress through the buffer.

	// Avoid reading sample D multiple times by discarding unread samples C, B, A.
	rr.tail = rr.head

	// Get the new head and starting reading from it.
	rr.head = atomic.LoadUint64(&rr.meta.Data_head)
	rr.read = rr.head

	if rr.tail-rr.head > uint64(cap(rr.ring)) {
		// ring has been fully written, only permit at most cap(rr.ring)
		// bytes to be read.
		rr.tail = rr.head + uint64(cap(rr.ring))
	}
}

func (rr *reverseReader) size() int {
	return len(rr.ring)
}

func (rr *reverseReader) writeTail() {
	// We do not care about tail for over writable perf buffer.
	// So, this function is noop.
}

func (rr *reverseReader) Read(p []byte) (int, error) {
	start := int(rr.read & rr.mask)

	n := len(p)
	// Truncate if the read wraps in the ring buffer
	if remainder := cap(rr.ring) - start; n > remainder {
		n = remainder
	}

	// Truncate if there isn't enough data
	if remainder := int(rr.tail - rr.read); n > remainder {
		n = remainder
	}

	copy(p, rr.ring[start:start+n])
	rr.read += uint64(n)

	if rr.read == rr.tail {
		return n, io.EOF
	}

	return n, nil
}
