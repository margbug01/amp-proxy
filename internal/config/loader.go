package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads a YAML configuration file from disk and returns a populated
// Config. Defaults are applied before the configuration is validated.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := Config{
		AmpCode: AmpCode{
			RestrictManagementToLocalhost: true,
		},
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config %q: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults fills in reasonable defaults for any unset scalar fields.
// Host and Port default independently so that a config with only `port:` set
// still binds loopback-only instead of silently binding all interfaces.
func (c *Config) applyDefaults() {
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 8787
	}
	if c.AmpCode.UpstreamURL == "" {
		c.AmpCode.UpstreamURL = "https://ampcode.com"
	}
}
