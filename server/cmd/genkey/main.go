// genkey prints a fresh 64-hex-char master key for the
// CARVILON_SECRETS_KEY environment variable (legacy alias:
// UNIFIX_SECRETS_KEY is still accepted by secrets.New). Run once
// per installation; the output should be stored in the operator's
// secret manager (or the systemd unit's EnvironmentFile).
package main

import (
	"fmt"
	"log"

	"carvilon.local/server/internal/secrets"
)

func main() {
	k, err := secrets.GenerateKeyHex()
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	fmt.Println(k)
}
