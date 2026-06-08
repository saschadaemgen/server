package proto

import (
	"fmt"
	"testing"
)

const (
	testUDMID    = "0cea14122cfd"
	testDeviceID = "0cea1442 4242"
	testFromID   = "abc123"
)

func TestMQTTBrokerEndpoint(t *testing.T) {
	if MQTTBrokerScheme != "tls" {
		t.Errorf("MQTTBrokerScheme = %q, want %q", MQTTBrokerScheme, "tls")
	}
	if MQTTBrokerPort != 12812 {
		t.Errorf("MQTTBrokerPort = %d, want 12812", MQTTBrokerPort)
	}
	url := fmt.Sprintf("%s://192.168.1.1:%d", MQTTBrokerScheme, MQTTBrokerPort)
	if url != "tls://192.168.1.1:12812" {
		t.Errorf("broker url = %q, want %q", url, "tls://192.168.1.1:12812")
	}
}

func TestMQTTTopicRPCRequest(t *testing.T) {
	got := fmt.Sprintf(MQTTTopicRPCRequest, testUDMID, testDeviceID)
	want := "/uctrl/0cea14122cfd/device/0cea1442 4242/rpc/+/request"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMQTTTopicRPCResponse(t *testing.T) {
	got := fmt.Sprintf(MQTTTopicRPCResponseTmpl, testUDMID, testDeviceID, testFromID)
	want := "/uctrl/0cea14122cfd/device/0cea1442 4242/rpc/abc123/response"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMQTTTopicHeartbeat(t *testing.T) {
	got := fmt.Sprintf(MQTTTopicHeartbeat, testUDMID, testDeviceID)
	want := "/uctrl/0cea14122cfd/device/0cea1442 4242/stat"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRPCMethodPaths(t *testing.T) {
	cases := map[string]string{
		"RPCMethodUpdateTokens":               "/update_tokens",
		"RPCMethodUpdateConfigs":              "/update_configs",
		"RPCMethodRemoteView":                 "/remote_view",
		"RPCMethodCancelDoorbellNotification": "/cancel_doorbell_notification",
		"RPCMethodUnlock":                     "/unlock",
		"RPCMethodImageCapture":               "/image/capture",
		"RPCMethodImageThumbnail":             "/image/thumbnail",
	}
	got := map[string]string{
		"RPCMethodUpdateTokens":               RPCMethodUpdateTokens,
		"RPCMethodUpdateConfigs":              RPCMethodUpdateConfigs,
		"RPCMethodRemoteView":                 RPCMethodRemoteView,
		"RPCMethodCancelDoorbellNotification": RPCMethodCancelDoorbellNotification,
		"RPCMethodUnlock":                     RPCMethodUnlock,
		"RPCMethodImageCapture":               RPCMethodImageCapture,
		"RPCMethodImageThumbnail":             RPCMethodImageThumbnail,
	}
	for k, want := range cases {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}
