package proto

// WebSocket notification endpoint for adopted devices.
const (
	WSScheme = "wss"
	WSPort   = 12443
	WSPath   = "/api/v2/ws/notification"
)

// JWT signing constants. The secret itself was extracted from the
// UDM heap during the initial reverse-engineering pass and lives
// outside this package (callers supply it explicitly).
const (
	JWTAlgorithm = "HS256"
	JWTIssuer    = "unifi-access"
	JWTLifetime  = 15 // days
)
