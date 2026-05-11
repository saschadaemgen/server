package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"unifix.local/mock/internal/crypto"
	"unifix.local/mock/internal/handlers"
	"unifix.local/mock/internal/identity"
	"unifix.local/mock/internal/stages/adoption"
	"unifix.local/mock/internal/stages/discovery"
	"unifix.local/mock/internal/stages/mqtt"
	"unifix.local/mock/internal/stages/websocket"
	"unifix.local/mock/internal/state"
)

type simpleLogger struct{}

func (simpleLogger) Infof(f string, a ...any)  { log.Printf("INFO  "+f, a...) }
func (simpleLogger) Warnf(f string, a ...any)  { log.Printf("WARN  "+f, a...) }
func (simpleLogger) Errorf(f string, a ...any) { log.Printf("ERROR "+f, a...) }

func main() {
	macFlag := flag.String("mac", "", "device MAC address (required), e.g. 0c:ea:14:42:42:42")
	ipFlag := flag.String("ipv4", "", "device IPv4 address (required)")
	portFlag := flag.Uint("service-port", 8080, "TLV 0x24 service port")
	nameFlag := flag.String("name", "", "device name (default derived from MAC)")
	guidFlag := flag.String("guid", "", "device GUID (default freshly generated)")
	showJWTFlag := flag.Bool("show-jwt", false, "sign and print a sample JWT, then exit")
	runFlag := flag.Bool("run", false, "run the mock daemon (otherwise prints identity and exits)")
	stateDirFlag := flag.String("state-dir", "./state", "base directory for per-mock state and certs")
	flag.Parse()

	if *macFlag == "" || *ipFlag == "" {
		log.Fatal("mock: --mac and --ipv4 are required")
	}

	mac, err := net.ParseMAC(*macFlag)
	if err != nil {
		log.Fatalf("mock: invalid --mac: %v", err)
	}

	ip := net.ParseIP(*ipFlag)
	if ip == nil || ip.To4() == nil {
		log.Fatalf("mock: invalid --ipv4: %s", *ipFlag)
	}

	id, err := identity.NewMockIdentity(mac, *nameFlag, *guidFlag, ip.To4(), uint16(*portFlag))
	if err != nil {
		log.Fatalf("mock: identity error: %v", err)
	}

	host, _ := os.Hostname()
	fmt.Printf("unifix mock starting host=%s go=%s\n", host, runtime.Version())
	fmt.Printf("identity: %s\n", id)

	if *showJWTFlag {
		token, err := crypto.SignJWT(id.ID)
		if err != nil {
			log.Fatalf("mock: jwt sign error: %v", err)
		}
		fmt.Printf("jwt: %s\n", token)
		return
	}

	if !*runFlag {
		return
	}

	store, err := state.New(*stateDirFlag)
	if err != nil {
		log.Fatalf("mock: state init: %v", err)
	}

	complete, err := store.BundleComplete(id.ID)
	if err != nil {
		log.Fatalf("mock: state check: %v", err)
	}

	var initialBundle *state.Bundle
	if complete {
		log.Printf("mock: bundle already complete for %s, skipping stage 4", id.ID)
		initialBundle, err = store.LoadBundle(id.ID)
		if err != nil {
			log.Fatalf("mock: load bundle: %v", err)
		}
	}

	log.Printf("starting stage 1 discovery listener")
	disc, err := discovery.New(id, simpleLogger{})
	if err != nil {
		log.Fatalf("mock: discovery init: %v", err)
	}
	defer disc.Close()

	var adoptSrv *adoption.Server
	if !complete {
		log.Printf("starting stage 4 adoption endpoint on :%d", id.ServicePort)
		bindAddr := fmt.Sprintf(":%d", id.ServicePort)
		certDir := store.CertDir(id.ID)
		adoptSrv, err = adoption.New(id, store, simpleLogger{}, bindAddr, certDir)
		if err != nil {
			log.Fatalf("mock: adoption init: %v", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = adoptSrv.Close(ctx)
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 4)
	go func() { errCh <- disc.Run(ctx) }()
	if adoptSrv != nil {
		go func() { errCh <- adoptSrv.Run(ctx) }()
	}

	go runStages56(ctx, id, store, initialBundle, adoptSrv, errCh)

	select {
	case <-ctx.Done():
		log.Println("mock: shutdown requested")
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("mock: stage failure: %v", err)
		}
	}
	log.Println("mock: shutdown clean")
}

// runStages56 waits for a complete adoption bundle (either
// provided at startup or signalled via AdoptedChan), then launches
// the WebSocket client (stage 5) and MQTT client (stage 6) in
// parallel.
func runStages56(
	ctx context.Context,
	id *identity.MockIdentity,
	store *state.Store,
	initial *state.Bundle,
	adoptSrv *adoption.Server,
	errCh chan<- error,
) {
	bundle := initial
	if bundle == nil {
		if adoptSrv == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-adoptSrv.AdoptedChan():
			b, err := store.LoadBundle(id.ID)
			if err != nil {
				log.Printf("mock: post-adopt load bundle: %v", err)
				return
			}
			if b == nil {
				log.Printf("mock: bundle missing after adoption")
				return
			}
			bundle = b
			log.Printf("mock: ADOPTION COMPLETE, bundle persisted to %s",
				filepath.Join(store.BaseDir(), id.ID, "bundle.json"))
		}
	}

	certDir := store.CertDir(id.ID)
	caCertPath := filepath.Join(certDir, "broker_ca.crt")

	wsClient, err := websocket.New(id, bundle, caCertPath, simpleLogger{})
	if err != nil {
		log.Printf("mock: ws init: %v", err)
		return
	}
	log.Printf("starting stage 5 websocket client")
	go func() { errCh <- wsClient.Run(ctx) }()

	mqttClient, err := mqtt.New(id, bundle, certDir, simpleLogger{})
	if err != nil {
		log.Printf("mock: mqtt init: %v", err)
		return
	}

	mh := &handlers.Handler{Store: store, MockID: id.ID, Log: simpleLogger{}}
	registry := mqtt.NewMethodRegistry(mqtt.DefaultHandler{}, simpleLogger{})
	registry.Register("/update_tokens", mh.UpdateTokens)
	registry.Register("/update_configs", mh.UpdateConfigs)
	registry.Register("/remote_view", mh.RemoteView)
	registry.Register("/cancel_doorbell_notification", mh.CancelDoorbell)
	mqttClient.SetHandler(registry)

	log.Printf("starting stage 6 mqtt client")
	errCh <- mqttClient.Run(ctx)
}
