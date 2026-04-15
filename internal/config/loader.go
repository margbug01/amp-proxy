package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads a YAML configuration file from disk and returns a populated
// Config. The caller is responsible for validating the resulting server
// bind fields if defaults are not acceptable.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

// applyDefaults fills in reasonable defaults for any unset scalar fields.
func (c *Config) applyDefaults() {
	if c.Host == "" && c.Port == 0 {
		c.Host = "127.0.0.1"
		c.Port = 8787
	}
	if c.AmpCode.UpstreamURL == "" {
		c.AmpCode.UpstreamURL = "https://ampcode.com"
	}
}
