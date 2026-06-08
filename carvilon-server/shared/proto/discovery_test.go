package proto

import (
	"bytes"
	"testing"
)

func TestDiscoveryProbeMagicLength(t *testing.T) {
	if got := len(DiscoveryProbeMagic); got != 4 {
		t.Errorf("DiscoveryProbeMagic length = %d, want 4", got)
	}
}

func TestDiscoveryProbeMagicBytes(t *testing.T) {
	want := []byte{0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(DiscoveryProbeMagic, want) {
		t.Errorf("DiscoveryProbeMagic = % x, want % x", DiscoveryProbeMagic, want)
	}
}

func TestDiscoveryConstants(t *testing.T) {
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"DiscoveryPort", DiscoveryPort, 10001},
		{"DiscoveryMulticastAddr", DiscoveryMulticastAddr, "233.89.188.1"},
		{"DiscoveryBroadcastAddr", DiscoveryBroadcastAddr, "192.168.1.255"},
		{"DiscoveryLimitedBcast", DiscoveryLimitedBcast, "255.255.255.255"},
		{"DiscoveryProbeInterval", DiscoveryProbeInterval, 10},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}
