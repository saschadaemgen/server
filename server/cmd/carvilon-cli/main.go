// carvilon-cli is the operator command-line companion to
// carvilon-server. Currently a single subcommand-tree:
//
//	carvilon-cli esp adopt --mac <MAC> --name <NAME>
//	                     [--intercom <MAC>] [--mieter <UA-USER-ID>]
//	                     [--db <PATH>]
//
// Saison 13-08 Phase A introduced this so the parallel ESP-Chat
// can issue and rotate ESP-Viewer bearer tokens without going
// through the /a/esp-viewers admin UI.
//
// More subcommands will land here as they are needed; the
// dispatcher is intentionally tiny rather than pulling in a
// CLI framework.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "esp":
		if err := runESP(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "esp: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: carvilon-cli <subcommand> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "subcommands:")
	fmt.Fprintln(w, "  esp adopt    adopt an ESP-Viewer + emit a fresh bearer token")
}
