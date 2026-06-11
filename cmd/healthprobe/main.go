// Tiny static binary used as the Docker HEALTHCHECK probe target.
// It does one http.Get against the configured URL and exits 0 on a
// 2xx, 1 otherwise. Stdlib only, CGO disabled, ~2MB binary.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: healthprobe <url> [timeout-seconds]")
		os.Exit(2)
	}
	timeoutSec := 3
	if len(os.Args) >= 3 {
		var t int
		_, _ = fmt.Sscanf(os.Args[2], "%d", &t)
		if t > 0 {
			timeoutSec = t
		}
	}
	c := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	r, err := c.Get(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthprobe: get:", err)
		os.Exit(1)
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "healthprobe: status %d\n", r.StatusCode)
		os.Exit(1)
	}
	fmt.Printf("ok %d\n", r.StatusCode)
}
