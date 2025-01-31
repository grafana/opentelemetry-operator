package config

import (
	"fmt"
)

const (
	configMapKey       = "config.yaml"
	configMapNamespace = "default"
	configMapName      = "auto-instrumentation-config"
)

type Config struct {
	// Discovery configuration
	Discovery DiscoveryConfig `yaml:"discovery"`
}

func (c *Config) Validate() error {
	if err := c.Discovery.Services.Validate(); err != nil {
		return fmt.Errorf("error in services YAML property: %w", err)
	}

	return nil
}
