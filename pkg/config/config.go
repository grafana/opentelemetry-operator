package config

import "fmt"

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
