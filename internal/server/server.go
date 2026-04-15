// Package server wires the amp-proxy HTTP server. It constructs a Gin
// engine, mounts the amp routing module, and provides a graceful-shutdown
// friendly Run loop.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/margbug01/amp-proxy/internal/amp"
	"github.com/margbug01/amp-proxy/internal/auth"
	"github.com/margbug01/amp-proxy/internal/config"
	"github.com/margbug01/amp-proxy/internal/customproxy"
	"github.com/margbug01/amp-proxy/internal/handlers"
	"github.com/margbug01/amp-proxy/internal/modules"
	sdkaccess "github.com/margbug01/amp-proxy/sdk/access"
)

// Server owns the HTTP server, the Gin engine, and the amp module instance.
type Server struct {
	cfg       *config.Config
	engine    *gin.Engine
	validator *auth.Validator
	ampModule *amp.AmpModule
	httpSrv   *http.Server
}

// New constructs a Server for the given configuration. Routes are registered
// immediately so that the engine is ready to serve once Run is invoked.
func New(cfg *config.Config) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("server: config is nil")
	}

	// Seed the custom provider registry before route registration so the
	// amp module's fallback handler can pick up provider matches on the
	// very first request.
	if err := customproxy.GetGlobal().Configure(cfg.AmpCode.CustomProviders); err != nil {
		return nil, fmt.Errorf("configure custom providers: %w", err)
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery(), accessLog(), bodyCapture("F:/AA/amp-proxy/capture", "/v1/responses"))

	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	validator := auth.NewValidator(cfg.APIKeys)

	ampModule := amp.New(
		amp.WithAccessManager(sdkaccess.NewManager()),
		amp.WithAuthMiddleware(validator.Middleware()),
	)

	modCtx := modules.Context{
		Engine:         engine,
		BaseHandler:    &handlers.BaseAPIHandler{},
		Config:         cfg,
		AuthMiddleware: validator.Middleware(),
	}
	if err := modules.RegisterModule(modCtx, ampModule); err != nil {
		return nil, fmt.Errorf("register amp module: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return &Server{
		cfg:       cfg,
		engine:    engine,
		validator: validator,
		ampModule: ampModule,
		httpSrv: &http.Server{
			Addr:              addr,
			Handler:           engine,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}, nil
}

// Addr returns the bound host:port string.
func (s *Server) Addr() string {
	return s.httpSrv.Addr
}

// Run starts the HTTP server and blocks until ctx is cancelled or the listener
// fails. On ctx cancellation, Run initiates a graceful shutdown with a 10s
// deadline.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		log.Infof("amp-proxy listening on %s", s.httpSrv.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// OnConfigUpdated replaces the live validator keys, refreshes the custom
// provider registry, and notifies the amp module. Intended to be invoked by
// a future hot-reload watcher.
func (s *Server) OnConfigUpdated(cfg *config.Config) error {
	s.cfg = cfg
	s.validator.SetKeys(cfg.APIKeys)
	if err := customproxy.GetGlobal().Configure(cfg.AmpCode.CustomProviders); err != nil {
		log.Errorf("custom provider reconfigure failed: %v", err)
	}
	return s.ampModule.OnConfigUpdated(cfg)
}
