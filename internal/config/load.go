package config

import (
	"fmt"
	"os"
	"path/filepath"
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
	// Only Load has a real file path to resolve relative fields (like
	// output.schema) against — Parse is also used to reconstruct a
	// config from YAML stored in SQLite, which has no source directory.
	cfg.SourceDir = filepath.Dir(path)
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
