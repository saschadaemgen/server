// Package proto encodes the reverse-engineered UniFi Access wire
// format: discovery TLVs, MQTT topics, WebSocket endpoint, and
// the protobuf-like RPC wire format.
package proto

// Discovery protocol constants.
//
// UDM probes devices via multicast 233.89.188.1:10001 and
// limited broadcast 255.255.255.255:10001 every 10 seconds with a
// 4-byte magic. Devices listen on 0.0.0.0:10001 with multicast
// membership joined and reply unicast to the UDM probe source port.

const (
	DiscoveryPort          = 10001
	DiscoveryMulticastAddr = "233.89.188.1"
	DiscoveryBroadcastAddr = "192.168.1.255"
	DiscoveryLimitedBcast  = "255.255.255.255"
	DiscoveryProbeInterval = 10 // seconds
)

// DiscoveryProbeMagic is the 4-byte payload UDM sends to discover devices.
var DiscoveryProbeMagic = []byte{0x01, 0x00, 0x00, 0x00}
