package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	fmt.Printf("license-server starting host=%s go=%s\n", hostname, runtime.Version())
	os.Exit(0)
}
