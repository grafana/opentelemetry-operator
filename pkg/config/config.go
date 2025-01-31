package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	v1 "k8s.io/api/core/v1"
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

func loadConfig(configMap *v1.ConfigMap) error {
	s := configMap.Data[configMapKey]
	config := Config{}
	err := yaml.Unmarshal([]byte(s), &config)
	return err
}
