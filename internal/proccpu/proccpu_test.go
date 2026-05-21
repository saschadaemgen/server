package proccpu

import (
	"runtime"
	"testing"
)

// TestNewSampler_DoesNotError covers both build paths: the Linux
// version reads /proc on construction, the stub returns immediately.
// Either way the constructor must not fail under normal circumstances.
func TestNewSampler_DoesNotError(t *testing.T) {
	s, err := NewSampler()
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	if s == nil {
		t.Fatal("NewSampler returned nil sampler")
	}
}

// TestSample_FirstCallReturnsNotOK is the documented contract: the
// first Sample() after construction returns (0, false) because there's
// no previous baseline to diff against.
func TestSample_FirstCallReturnsNotOK(t *testing.T) {
	s, err := NewSampler()
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	_, ok := s.Sample()
	if ok {
		t.Errorf("first Sample returned ok=true; want false (no baseline yet)")
	}
}

// TestSample_NonLinuxAlwaysNotOK locks the stub contract.
func TestSample_NonLinuxAlwaysNotOK(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux has the real sampler; this asserts the stub-only behaviour")
	}
	s, _ := NewSampler()
	if _, ok := s.Sample(); ok {
		t.Errorf("non-Linux Sample should always return ok=false")
	}
	// Repeat — must remain false (the stub has no state to become "ok").
	if _, ok := s.Sample(); ok {
		t.Errorf("repeated non-Linux Sample should remain ok=false")
	}
}

// TestSample_NilSamplerSafe covers the documented "if NewSampler errored
// you can still pass nil and the call sites stay panic-free" path.
func TestSample_NilSamplerSafe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the nil-receiver branch only exists on the Linux sampler; stub uses value receiver")
	}
	var s *Sampler
	if _, ok := s.Sample(); ok {
		t.Errorf("nil sampler should return ok=false, not panic")
	}
}
