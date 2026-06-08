// Saison 13-08 Phase A: 'esp' subcommand tree of carvilon-cli.
// Currently only 'adopt' lives here; later phases can grow
// 'rotate-token' / 'list' / 'revoke' next to it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"carvilon.local/server/internal/auth/esptoken"
	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/viewerstore"
)

// macFormat matches lowercase colon-form MACs (e.g. 0c:ea:14:42:42:42).
var macFormat = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

func runESP(args []string) error {
	if len(args) < 1 {
		return errors.New("missing 'esp' subcommand; try 'carvilon-cli esp adopt --help'")
	}
	switch args[0] {
	case "adopt":
		return runESPAdopt(args[1:], os.Stdout)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, "usage: carvilon-cli esp <command> [args]")
		fmt.Fprintln(os.Stdout, "")
		fmt.Fprintln(os.Stdout, "commands:")
		fmt.Fprintln(os.Stdout, "  adopt    adopt an ESP-Viewer + emit a fresh bearer token")
		return nil
	default:
		return fmt.Errorf("unknown esp subcommand %q", args[0])
	}
}

// runESPAdopt parses flags + writes a fresh ESP-Viewer row,
// printing the plain bearer token to out exactly once.
//
// out is injected so tests can capture stdout deterministically.
func runESPAdopt(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("esp adopt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mac := fs.String("mac", "", "ESP MAC in lowercase colon form, e.g. 0c:ea:14:aa:bb:cc")
	name := fs.String("name", "", "Wohnungs-Name (display name)")
	intercom := fs.String("intercom", "", "paired intercom MAC (optional)")
	mieter := fs.String("mieter", "", "linked UA-User ID (optional)")
	dbPath := fs.String("db", "", "SQLite DB path (default: $CARVILON_DB_PATH or legacy $UNIFIX_DB_PATH or ./state/carvilon.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	macLower := strings.ToLower(strings.TrimSpace(*mac))
	if macLower == "" {
		return errors.New("--mac is required")
	}
	if !macFormat.MatchString(macLower) {
		return fmt.Errorf("--mac %q must be lowercase colon-form (xx:xx:xx:xx:xx:xx)", *mac)
	}
	nameTrimmed := strings.TrimSpace(*name)
	if nameTrimmed == "" || len(nameTrimmed) > 64 {
		return errors.New("--name is required and must be <= 64 chars")
	}
	intercomLower := strings.ToLower(strings.TrimSpace(*intercom))
	if intercomLower != "" && !macFormat.MatchString(intercomLower) {
		return fmt.Errorf("--intercom %q must be lowercase colon-form", *intercom)
	}
	mieterTrimmed := strings.TrimSpace(*mieter)

	resolvedDB := *dbPath
	if resolvedDB == "" {
		resolvedDB = config.FromEnv().DBPath
	}

	d, err := db.Open(resolvedDB)
	if err != nil {
		return fmt.Errorf("open db %q: %w", resolvedDB, err)
	}
	defer d.Close()

	clear, hash, err := esptoken.Generate()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}

	port, err := viewerstore.NextFreeServicePort(context.Background(), d.DB)
	if err != nil {
		return fmt.Errorf("allocate service port: %w", err)
	}

	if err := viewerstore.Insert(context.Background(), d.DB, viewerstore.InsertSpec{
		MAC:               macLower,
		Name:              nameTrimmed,
		ServicePort:       port,
		Type:              "esp",
		DeviceTokenHash:      hash,
		PairedIntercomMAC: intercomLower,
		LinkedUAUserID:    mieterTrimmed,
	}, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("insert viewer: %w", err)
	}

	fmt.Fprintln(out, "ESP-Viewer adopted.")
	fmt.Fprintf(out, "  mac:               %s\n", macLower)
	fmt.Fprintf(out, "  name:              %s\n", nameTrimmed)
	fmt.Fprintf(out, "  service_port:      %d\n", port)
	if intercomLower != "" {
		fmt.Fprintf(out, "  paired_intercom:   %s\n", intercomLower)
	}
	if mieterTrimmed != "" {
		fmt.Fprintf(out, "  linked_ua_user_id: %s\n", mieterTrimmed)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Bearer token (will not be shown again):")
	fmt.Fprintln(out, clear)
	return nil
}

