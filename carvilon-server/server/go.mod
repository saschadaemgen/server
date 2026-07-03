module carvilon.local/server

go 1.26.1

replace carvilon.local/shared => ../shared

replace carvilon.local/mock => ../mock

replace carvilon.local/stream => ../../streaming-server

require (
	carvilon.local/mock v0.0.0-00010101000000-000000000000
	carvilon.local/stream v0.0.0-00010101000000-000000000000
	github.com/coder/websocket v1.8.14
	github.com/creack/pty v1.1.24
	github.com/eclipse/paho.mqtt.golang v1.5.0
	github.com/hashicorp/mdns v1.0.6
	github.com/mochi-mqtt/server/v2 v2.7.9
	github.com/pion/webrtc/v4 v4.2.12
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/warthog618/go-gpiocdev v0.9.1
	golang.org/x/crypto v0.51.0
	golang.org/x/oauth2 v0.36.0
	modernc.org/sqlite v1.50.1
)

require (
	carvilon.local/shared v0.0.0 // indirect
	cloud.google.com/go/compute/metadata v0.3.0 // indirect
	github.com/bluenviron/gortsplib/v5 v5.5.3 // indirect
	github.com/bluenviron/mediacommon/v2 v2.8.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/miekg/dns v1.1.55 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pion/datachannel v1.6.0 // indirect
	github.com/pion/dtls/v3 v3.1.2 // indirect
	github.com/pion/ice/v4 v4.2.5 // indirect
	github.com/pion/interceptor v0.1.44 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.2 // indirect
	github.com/pion/sctp v1.9.5 // indirect
	github.com/pion/sdp/v3 v3.0.18 // indirect
	github.com/pion/srtp/v3 v3.0.10 // indirect
	github.com/pion/stun/v3 v3.1.2 // indirect
	github.com/pion/transport/v4 v4.0.1 // indirect
	github.com/pion/turn/v4 v4.1.4 // indirect
	github.com/pion/turn/v5 v5.0.3 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/xid v1.4.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
