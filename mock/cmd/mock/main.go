// mock is the stand-alone runner for the UA Intercom Viewer
// simulator. It is preserved for saison-research and stage tests;
// production deployment embeds the same code as a library inside
// carvilon-server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"carvilon.local/mock"
)

func main() {
	cfg := mock.Config{}
	flag.StringVar(&cfg.MAC, "mac", "", "device MAC address (required), e.g. 0c:ea:14:42:42:42")
	flag.StringVar(&cfg.IPv4, "ipv4", "", "device IPv4 address (required)")
	flag.StringVar(&cfg.Name, "name", "", "device name (default derived from MAC)")
	var port uint
	flag.UintVar(&port, "service-port", 8080, "TLV 0x24 service port")
	flag.StringVar(&cfg.GUID, "guid", "", "device GUID (default freshly generated)")
	flag.StringVar(&cfg.StateDir, "state-dir", "./state", "base directory for per-mock state and certs")
	showJWT := flag.Bool("show-jwt", false, "sign and print a sample JWT, then exit")
	run := flag.Bool("run", false, "run the mock daemon (otherwise prints identity and exits)")
	flag.Parse()
	cfg.ServicePort = uint16(port)

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := cfg.Validate(); err != nil {
		log.Error("config invalid", "err", err)
		os.Exit(1)
	}

	if *showJWT {
		token, err := mock.GenerateJWT(cfg)
		if err != nil {
			log.Error("jwt sign failed", "err", err)
			os.Exit(1)
		}
		fmt.Printf("jwt: %s\n", token)
		return
	}

	v, err := mock.New(cfg, log)
	if err != nil {
		log.Error("viewer new failed", "err", err)
		os.Exit(1)
	}

	host, _ := os.Hostname()
	fmt.Printf("carvilon mock starting host=%s go=%s\n", host, runtime.Version())
	fmt.Printf("identity: %s\n", v.Identity())

	if !*run {
		log.Info("identity printed, not running", "mac", v.MAC())
		return
	}

	// In stand-alone mode the only consumer is this logger. The
	// embedded library path inside carvilon-server reads the same
	// channels through the mock manager.
	go func() {
		for ev := range v.Events() {
			log.Info("doorbell event received",
				"mac", ev.ViewerMAC,
				"request_id", ev.RequestID,
				"device_id", ev.DeviceID,
				"room_id", ev.RoomID,
			)
		}
	}()
	go func() {
		for ev := range v.Cancels() {
			log.Info("doorbell cancel received",
				"mac", ev.ViewerMAC,
				"cancel_token", ev.CancelToken,
				"reason", ev.ReasonCode,
			)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := v.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("viewer run failed", "err", err)
		os.Exit(1)
	}
	log.Info("mock shutdown clean")
}
