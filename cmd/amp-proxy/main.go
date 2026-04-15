// amp-proxy is a focused reverse proxy for Sourcegraph Amp (ampcode.com).
// See README.md and NOTICE.md for scope and derivation details.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	log "github.com/sirupsen/logrus"

	"github.com/margbug01/amp-proxy/internal/config"
	"github.com/margbug01/amp-proxy/internal/server"
)

// Build-time version metadata. These are populated by the release pipeline
// via -ldflags "-X main.version=... -X main.commit=... -X main.date=..."
// and fall back to "dev" for `go run`/`go build` builds.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("amp-proxy %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	log.SetLevel(log.InfoLevel)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("construct server: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server run: %v", err)
	}
	log.Info("amp-proxy shut down cleanly")
}
