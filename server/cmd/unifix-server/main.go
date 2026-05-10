package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

func main() {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	fmt.Printf("unifix-server starting host=%s go=%s build_time=%s\n",
		hostname, runtime.Version(), time.Now().UTC().Format(time.RFC3339))
	os.Exit(0)
}
