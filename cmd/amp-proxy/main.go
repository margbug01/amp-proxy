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
	"time"

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
	// Subcommand dispatch. Keep this strictly ahead of flag.Parse so the
	// subcommand's own flag set can own its argv slice.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if err := runInit(os.Args[2:]); err != nil {
			log.Fatalf("init: %v", err)
		}
		return
	}

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
	go watchConfig(ctx, *configPath, srv)

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server run: %v", err)
	}
	log.Info("amp-proxy shut down cleanly")
}

func watchConfig(ctx context.Context, path string, srv *server.Server) {
	info, err := os.Stat(path)
	if err != nil {
		log.Errorf("config watcher: stat %s: %v", path, err)
		return
	}
	lastMod := info.ModTime()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				log.Errorf("config watcher: stat %s: %v", path, err)
				continue
			}
			modTime := info.ModTime()
			if modTime.Equal(lastMod) {
				continue
			}
			lastMod = modTime
			cfg, err := config.Load(path)
			if err != nil {
				log.Errorf("config watcher: reload %s: %v", path, err)
				continue
			}
			if err := srv.OnConfigUpdated(cfg); err != nil {
				log.Errorf("config watcher: apply %s: %v", path, err)
				continue
			}
			log.Infof("config reloaded from %s", path)
		}
	}
}
