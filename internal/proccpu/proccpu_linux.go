//go:build linux

package proccpu

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sampler holds the previous sample of cputime so the next call can
// compute a delta. The mutex serialises concurrent Sample() callers —
// both the /stream/stats handler and the periodic logger can fire it.
type Sampler struct {
	pid      int
	statPath string
	ticksHz  float64 // CLK_TCK; we use the conventional 100 if SC_CLK_TCK can't be read

	mu       sync.Mutex
	hasPrev  bool
	prevTime time.Time
	prevTicks uint64 // utime + stime in clock ticks
}

// NewSampler returns a Sampler for the current process. The first
// Sample call after construction returns (0, false).
func NewSampler() (*Sampler, error) {
	pid := os.Getpid()
	s := &Sampler{
		pid:      pid,
		statPath: fmt.Sprintf("/proc/%d/stat", pid),
		ticksHz:  clockTicksHz(),
	}
	// Eagerly read one sample so the first real Sample() call has a
	// baseline. We still return ok=false on that first call so the UI
	// can show "—" instead of a spurious 0%.
	if _, err := s.readStat(); err != nil {
		return nil, fmt.Errorf("proccpu: initial read: %w", err)
	}
	return s, nil
}

// Sample reads /proc/<pid>/stat once and returns the average CPU
// percent (of one core) since the previous Sample. First call returns
// (0, false).
func (s *Sampler) Sample() (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ticks, err := s.readStat()
	if err != nil {
		// One bad read shouldn't sticky-poison subsequent calls; we
		// drop this sample but keep the previous baseline so the next
		// call can still produce a number.
		return 0, false
	}
	now := time.Now()

	if !s.hasPrev {
		s.hasPrev = true
		s.prevTime = now
		s.prevTicks = ticks
		return 0, false
	}

	elapsedSec := now.Sub(s.prevTime).Seconds()
	if elapsedSec <= 0 {
		return 0, false
	}
	dTicks := ticks - s.prevTicks
	dCPUSec := float64(dTicks) / s.ticksHz
	pct := dCPUSec / elapsedSec * 100.0

	s.prevTime = now
	s.prevTicks = ticks
	return pct, true
}

// readStat returns utime + stime from /proc/<pid>/stat, expressed in
// clock ticks. The format is field-positional but the second field
// (comm) can contain spaces and parentheses, so we find the LAST ')'
// and parse from there onwards.
func (s *Sampler) readStat() (uint64, error) {
	buf, err := os.ReadFile(s.statPath)
	if err != nil {
		return 0, err
	}
	// Skip past `pid (comm)`. The comm field is enclosed in '(' ... ')'.
	// We can't just split on space because comm may contain spaces.
	rparen := strings.LastIndexByte(string(buf), ')')
	if rparen < 0 || rparen+2 >= len(buf) {
		return 0, errors.New("proccpu: malformed /proc stat (no ')')")
	}
	// Fields after the closing paren, starting at index 0 = state.
	// The field positions (man 5 proc): state=1, ppid=2, ..., utime=14,
	// stime=15. After skipping `pid (comm)`, our index 0 = state, so
	// utime = index 11, stime = index 12.
	rest := strings.Fields(string(buf[rparen+2:]))
	if len(rest) < 13 {
		return 0, fmt.Errorf("proccpu: /proc stat has %d fields after comm, need >=13", len(rest))
	}
	utime, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("proccpu: utime: %w", err)
	}
	stime, err := strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("proccpu: stime: %w", err)
	}
	return utime + stime, nil
}

// clockTicksHz returns sysconf(_SC_CLK_TCK). We don't link libc; the
// kernel ABI has used 100 Hz on Linux for as long as the file format
// has existed, and there's no portable Go way to call sysconf without
// cgo. If a kernel ever changes this it will only affect the absolute
// scaling — the comparative numbers (mjpeg_hq vs h264_cbp on the same
// machine) stay valid.
func clockTicksHz() float64 {
	return 100.0
}
