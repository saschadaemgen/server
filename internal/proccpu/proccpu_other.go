//go:build !linux

package proccpu

// Sampler is a no-op outside Linux. The Linux-only /proc/<pid>/stat
// path doesn't exist on Windows / macOS / BSD; reporting a portable
// CPU percentage would require a real platform-specific implementation
// (Job Object on Windows, mach_task_basic_info on macOS, …) that we
// don't need for the S6-01 measurement campaign — the campaign runs on
// the production Linux UDM target.
//
// Sample always returns (0, false) so consumers (/stream/stats + the
// periodic logger) skip the CPU display on dev machines without
// branching on runtime.GOOS at the call site.
type Sampler struct{}

// NewSampler returns a no-op Sampler. The error return mirrors the
// Linux signature so callers don't have a per-platform switch.
func NewSampler() (*Sampler, error) { return &Sampler{}, nil }

// Sample always returns (0, false) on non-Linux builds.
func (s *Sampler) Sample() (float64, bool) { return 0, false }
