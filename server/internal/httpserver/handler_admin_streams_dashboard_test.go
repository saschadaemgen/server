package httpserver

import "testing"

func TestDeviceKind(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"127.0.0.1:55658", "loop"},
		{"[::1]:8080", "loop"},
		{"192.168.1.28:58949", "esp"},
		{"192.168.1.187:61669", "esp"}, // coarse rule: 192.168.x -> esp
		{"10.0.0.5:1234", "web"},
		{"172.16.4.9:443", "web"},
		{"203.0.113.7:5000", "web"},
		{"192.168.1.28", "esp"}, // no port still classifies
	}
	for _, c := range cases {
		if got := deviceKind(c.addr); got != c.want {
			t.Errorf("deviceKind(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
