//go:build !linux

package sysmetrics

// platformMetrics has nothing to offer off Linux: /proc, /sys and statfs
// telemetry are Linux-only, so the system category never surfaces on the
// dev machine (Windows/macOS) or any non-Linux host.
func platformMetrics() []metricDef { return nil }
