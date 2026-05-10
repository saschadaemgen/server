package proto

// WebSocket notification endpoint for adopted devices (saison 8).
const (
	WSScheme = "wss"
	WSPort   = 12443
	WSPath   = "/api/v2/ws/notification"
)

// JWT signing constants (saison 8 reverse engineering from UDM heap).
const (
	JWTAlgorithm = "HS256"
	JWTIssuer    = "unifi-access"
	JWTLifetime  = 15 // days
)
