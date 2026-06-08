package proto

// MQTT broker endpoint.
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

// RPC method paths sent by UDM to device, observed in live MQTT
// captures.
const (
	RPCMethodUpdateTokens               = "/update_tokens"
	RPCMethodUpdateConfigs              = "/update_configs"
	RPCMethodRemoteView                 = "/remote_view"
	RPCMethodCancelDoorbellNotification = "/cancel_doorbell_notification"
)

// UNVERIFIED: hypothetical method names that have NEVER been
// observed on the wire. They were proposed as plausible
// "device -> UDM" paths during early reverse engineering and
// kept around for orientation, but the real unlock path lives in
// the relay flow (see relay-related code in the mock package).
// Do NOT rely on these values for production wiring; treat them
// as TODO markers until a live capture confirms the schema.
const (
	RPCMethodUnlock         = "/unlock"
	RPCMethodImageCapture   = "/image/capture"
	RPCMethodImageThumbnail = "/image/thumbnail"
)
