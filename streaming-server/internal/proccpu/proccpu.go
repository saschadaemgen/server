// Package proccpu measures the streaming-server process's own CPU
// utilisation as a percentage of one core (so 100.0 == one core fully
// busy; 400.0 on a quad-core means every core is pinned).
//
// Purpose (S6-01): the ESP measurement campaign has to put a number on
// the transcoder cost. A profile that produces a slightly smaller MJPEG
// but doubles the ffmpeg CPU is a worse trade than the numbers in
// /stream/stats alone would suggest. CPU is reported as a sibling
// global field so the comparison stays direct.
//
// The Linux implementation is the real one: it parses
// /proc/<pid>/stat for utime + stime (in clock ticks) plus the
// wall-clock between samples, and reports the rate at which the
// process consumed CPU ticks. The non-Linux build is a stub returning
// 0 so the same call site compiles on the developer's Windows /
// macOS box without a `runtime.GOOS == "linux"` ifce surface check at
// every consumer.
//
// One sample is required to seed the differential measurement — the
// first Sample after [NewSampler] returns 0; the second and onwards
// return the average over the elapsed interval.
package proccpu

// Sampler is the public type used by callers. The concrete fields
// (the OS-specific state) live in proccpu_linux.go and
// proccpu_other.go.
//
// Construction: [NewSampler]. The zero value is NOT ready — it has
// no PID and would parse the wrong file on Linux.
//
// Cost: a single sample on Linux reads /proc/<pid>/stat, ~100 bytes,
// once per call. Cheap enough to fire on every /stream/stats GET; the
// periodic logger samples at most a few times a minute.

// Sample returns the average CPU percent (of one core) the process
// has consumed since the previous Sample call. The very first call
// after construction returns 0 (no previous sample to diff against).
//
// On non-Linux builds this always returns 0 and false.
//
//	pct, ok := s.Sample()
//	if !ok { /* skip — first call or unsupported platform */ }
//
// The "ok" flag is the only signal callers should use to decide
// whether to render the number; do not check runtime.GOOS at the
// call site.
//
// Method declarations are split across the OS-specific files so each
// build only compiles one body for Sampler.
