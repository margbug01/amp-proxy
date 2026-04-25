package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAcceptsValidConfig(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "port too low",
			mutate: func(c *Config) {
				c.Port = 0
			},
			wantErr: "port",
		},
		{
			name: "port too high",
			mutate: func(c *Config) {
				c.Port = 65536
			},
			wantErr: "port",
		},
		{
			name: "missing api keys",
			mutate: func(c *Config) {
				c.APIKeys = nil
			},
			wantErr: "api-keys",
		},
		{
			name: "blank api key",
			mutate: func(c *Config) {
				c.APIKeys = []string{"valid", "  "}
			},
			wantErr: "api-keys[1]",
		},
		{
			name: "relative upstream url",
			mutate: func(c *Config) {
				c.AmpCode.UpstreamURL = "ampcode.com"
			},
			wantErr: "ampcode.upstream-url",
		},
		{
			name: "invalid gemini route mode",
			mutate: func(c *Config) {
				c.AmpCode.GeminiRouteMode = "proxy"
			},
			wantErr: "gemini-route-mode",
		},
		{
			name: "custom provider missing name",
			mutate: func(c *Config) {
				c.AmpCode.CustomProviders[0].Name = " "
			},
			wantErr: "custom-providers[0].name",
		},
		{
			name: "custom provider relative url",
			mutate: func(c *Config) {
				c.AmpCode.CustomProviders[0].URL = "/v1"
			},
			wantErr: "custom-providers[0].url",
		},
		{
			name: "custom provider missing models",
			mutate: func(c *Config) {
				c.AmpCode.CustomProviders[0].Models = nil
			},
			wantErr: "custom-providers[0].models",
		},
		{
			name: "custom provider blank model",
			mutate: func(c *Config) {
				c.AmpCode.CustomProviders[0].Models = []string{"model", "\t"}
			},
			wantErr: "custom-providers[0].models[1]",
		},
		{
			name: "upstream api key entry missing upstream key",
			mutate: func(c *Config) {
				c.AmpCode.UpstreamAPIKeys[0].UpstreamAPIKey = " "
			},
			wantErr: "upstream-api-keys[0].upstream-api-key",
		},
		{
			name: "upstream api key entry missing client keys",
			mutate: func(c *Config) {
				c.AmpCode.UpstreamAPIKeys[0].APIKeys = nil
			},
			wantErr: "upstream-api-keys[0].api-keys",
		},
		{
			name: "upstream api key entry blank client key",
			mutate: func(c *Config) {
				c.AmpCode.UpstreamAPIKeys[0].APIKeys = []string{"local-key", " "}
			},
			wantErr: "upstream-api-keys[0].api-keys[1]",
		},
		{
			name: "duplicate custom provider model",
			mutate: func(c *Config) {
				c.AmpCode.CustomProviders = append(c.AmpCode.CustomProviders, CustomProvider{Name: "other", URL: "http://localhost:8081/v1", Models: []string{"MODEL"}})
			},
			wantErr: "duplicates model",
		},
		{
			name: "upstream api key entry unknown client key",
			mutate: func(c *Config) {
				c.AmpCode.UpstreamAPIKeys[0].APIKeys = []string{"other-local-key"}
			},
			wantErr: "must match a top-level api-keys entry",
		},
		{
			name: "duplicate upstream client key",
			mutate: func(c *Config) {
				c.AmpCode.UpstreamAPIKeys = append(c.AmpCode.UpstreamAPIKeys, AmpUpstreamAPIKeyEntry{UpstreamAPIKey: "upstream-2", APIKeys: []string{"local-key"}})
			},
			wantErr: "duplicates client key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestLoadAppliesDefaultsThenValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("api-keys:\n  - local-key\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want default", cfg.Host)
	}
	if cfg.Port != 8787 {
		t.Fatalf("Port = %d, want default", cfg.Port)
	}
	if cfg.AmpCode.UpstreamURL != "https://ampcode.com" {
		t.Fatalf("UpstreamURL = %q, want default", cfg.AmpCode.UpstreamURL)
	}
	if !cfg.AmpCode.RestrictManagementToLocalhost {
		t.Fatal("RestrictManagementToLocalhost = false, want default true")
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("port: 70000\napi-keys:\n  - local-key\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	if !strings.Contains(err.Error(), "validate config") || !strings.Contains(err.Error(), "port") {
		t.Fatalf("Load() error = %q, want validation port error", err.Error())
	}
}

func validConfig() *Config {
	return &Config{
		Host:    "127.0.0.1",
		Port:    8787,
		APIKeys: []string{"local-key"},
		AmpCode: AmpCode{
			UpstreamURL:     "https://ampcode.com",
			GeminiRouteMode: "translate",
			CustomProviders: []CustomProvider{
				{
					Name:   "gateway",
					URL:    "http://localhost:8080/v1",
					Models: []string{"model"},
				},
			},
			UpstreamAPIKeys: []AmpUpstreamAPIKeyEntry{
				{
					UpstreamAPIKey: "upstream-key",
					APIKeys:        []string{"local-key"},
				},
			},
		},
	}
}
