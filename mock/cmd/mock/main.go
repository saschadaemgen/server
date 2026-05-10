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
	"runtime"
	"syscall"

	"unifix.local/mock/internal/crypto"
	"unifix.local/mock/internal/identity"
	"unifix.local/mock/internal/stages/discovery"
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

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("starting stage 1 discovery listener for %s", id)
	lst, err := discovery.New(id, simpleLogger{})
	if err != nil {
		log.Fatalf("mock: discovery listener init: %v", err)
	}
	defer lst.Close()

	if err := lst.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("mock: discovery listener run: %v", err)
	}
	log.Println("mock: shutdown clean")
}
