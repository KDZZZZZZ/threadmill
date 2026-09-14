package threadmill

import _ "embed"

//go:embed threadmill.yaml
var defaultConfigYAML string

// DefaultConfigYAML returns the complete configuration compiled into Threadmill.
func DefaultConfigYAML() string {
	return defaultConfigYAML
}
