module carvilon.local/mock

go 1.26

require carvilon.local/shared v0.0.0

require (
	github.com/eclipse/paho.mqtt.golang v1.5.0
	nhooyr.io/websocket v1.8.10
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
)

replace carvilon.local/shared => ../shared
