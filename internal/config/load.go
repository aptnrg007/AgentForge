package config

import (
	"fmt"
	"os"
	"reflect"

	"sigs.k8s.io/yaml"
)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

func Parse(raw []byte) (*Config, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, err
	}

	if err := interpolateEnv(reflect.ValueOf(&cfg).Elem()); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}
