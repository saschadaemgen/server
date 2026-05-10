package proto

// MQTT broker endpoint (saison 8 + 9).
const (
	MQTTBrokerScheme = "tls" // UDM uses "tls://" prefix in broker_address
	MQTTBrokerPort   = 12812
)

// MQTT topic templates. Use fmt.Sprintf with udmID and deviceID
// (both MAC addresses without colons).
const (
	MQTTTopicRPCRequest      = "/uctrl/%s/device/%s/rpc/+/request"
	MQTTTopicRPCResponseTmpl = "/uctrl/%s/device/%s/rpc/%s/response"
	MQTTTopicHeartbeat       = "/uctrl/%s/device/%s/stat"
)

// RPC method paths sent by UDM to device (saison 9 live capture).
const (
	RPCMethodUpdateTokens               = "/update_tokens"
	RPCMethodUpdateConfigs              = "/update_configs"
	RPCMethodRemoteView                 = "/remote_view"
	RPCMethodCancelDoorbellNotification = "/cancel_doorbell_notification"
)

// RPC method paths sent by device to UDM (saison 9 hypothesized,
// not yet live-verified).
const (
	RPCMethodUnlock         = "/unlock"
	RPCMethodImageCapture   = "/image/capture"
	RPCMethodImageThumbnail = "/image/thumbnail"
)
