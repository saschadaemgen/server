package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"

	"unifix.local/mock/internal/crypto"
	"unifix.local/mock/internal/identity"
)

func main() {
	macFlag := flag.String("mac", "", "device MAC address (required), e.g. 0c:ea:14:42:42:42")
	ipFlag := flag.String("ipv4", "", "device IPv4 address (required)")
	portFlag := flag.Uint("service-port", 8080, "TLV 0x24 service port")
	nameFlag := flag.String("name", "", "device name (default derived from MAC)")
	guidFlag := flag.String("guid", "", "device GUID (default freshly generated)")
	showJWTFlag := flag.Bool("show-jwt", false, "sign and print a sample JWT, then exit")
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
}
