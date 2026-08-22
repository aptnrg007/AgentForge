package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
)

var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolateEnv walks every string reachable from v (struct fields, slice
// elements, map values) and replaces ${VAR} references with the named
// environment variable. It operates on already-decoded Go strings, never on
// raw YAML/JSON text, so a token containing quote or backslash characters
// can't corrupt the document it's substituted into.
func interpolateEnv(v reflect.Value) error {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return interpolateEnv(v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := interpolateEnv(v.Field(i)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		// A []byte-kinded slice (json.RawMessage, e.g.
		// ToolDefinition.InputSchema) is JSON Schema text, not agent
		// config strings — walking it byte by byte would be pointless,
		// and ${VAR} must not be allowed to expand inside a schema
		// document.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := interpolateEnv(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			elem := v.MapIndex(key)
			if elem.Kind() != reflect.String {
				continue
			}
			resolved, err := resolveEnvRefs(elem.String())
			if err != nil {
				return err
			}
			v.SetMapIndex(key, reflect.ValueOf(resolved))
		}
	case reflect.String:
		if !v.CanSet() {
			return nil
		}
		resolved, err := resolveEnvRefs(v.String())
		if err != nil {
			return err
		}
		v.SetString(resolved)
	}
	return nil
}

func resolveEnvRefs(s string) (string, error) {
	matches := envVarPattern.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s, nil
	}

	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(s[last:m[0]])
		name := s[m[2]:m[3]]
		val, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %q is not set", name)
		}
		b.WriteString(val)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String(), nil
}
