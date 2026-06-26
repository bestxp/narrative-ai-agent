// Tiny static binary used as the Docker HEALTHCHECK probe target.
// It does one http.Get against the configured URL and exits 0 on a
// 2xx, 1 otherwise. Stdlib only, CGO disabled, ~2MB binary.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// healthprobe is an internal k8s/Docker HEALTHCHECK binary. The URL
	// comes from the operator-supplied HEALTHCHECK argument and is
	// always an http(s) endpoint pointing at the local service. We
	// still validate scheme to fail loudly on misconfiguration rather
	// than silently leaking SSRF-able inputs.
	u, err := url.Parse(os.Args[1])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintln(os.Stderr, "healthprobe: invalid url:", os.Args[1])
		os.Exit(1)
	}

	//nolint:gosec // request URL is operator-supplied healthcheck target with validated scheme/host
	// request URL is operator-supplied healthcheck target; scheme/host are validated above before the request is built.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthprobe: request:", err)
		os.Exit(1)
	}

	r, err := c.Do(req) //nolint:gosec // request URL is operator-supplied healthcheck target with validated scheme/host
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthprobe: get:", err)
		os.Exit(1)
	}

	func() {
		defer func() { _ = r.Body.Close() }()

		if r.StatusCode < 200 || r.StatusCode >= 400 {
			fmt.Fprintf(os.Stderr, "healthprobe: status %d\n", r.StatusCode)
			os.Exit(1)
		}

		fmt.Printf("ok %d\n", r.StatusCode) //nolint:forbidigo // healthprobe stdout contract: "ok <code>"
	}()
}
