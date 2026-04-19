package soda

import (
	"github.com/i2y/ramune/workers"
)

// loadSodaTOML reads soda.toml from path. Returns (nil, nil) if the
// file does not exist (the TOML file is optional).
//
// The schema matches ramune.toml one-for-one, so we reuse the parser
// in ramune/workers to stay in sync with the standalone runtime.
func loadSodaTOML(path string) (*workers.RamuneTOML, error) {
	return workers.LoadRamuneTOML(path)
}

// applySodaTOML merges fields from soda.toml into cfg. Go Config values
// take precedence: a field already populated programmatically is left
// alone, the TOML value fills unset ones.
func applySodaTOML(cfg *Config, s *workers.RamuneTOML) error {
	if s == nil {
		return nil
	}
	derived, err := workers.ApplyRamuneTOML(s)
	if err != nil {
		return err
	}
	if len(cfg.NPMPackages) == 0 && len(derived.Dependencies) > 0 {
		cfg.NPMPackages = derived.Dependencies
	}
	if cfg.Permissions == nil && derived.Permissions != nil {
		cfg.Permissions = derived.Permissions
	}
	return nil
}

// buildExtraEnvBindingsJS returns the JS that installs a
// __extraEnvBindings(env) function so __buildEnv can fold named KV
// bindings onto env. Empty string when no KV bindings are declared.
//
// We reuse ramune/workers' builder, which produces a compatible
// snippet: both Soda and ramune/workers rely on the same
// globalThis.__extraEnvBindings hook.
func buildExtraEnvBindingsJS(kv []workers.TOMLKVBinding) string {
	if len(kv) == 0 {
		return ""
	}
	return workers.BuildExtraEnvJS(kv)
}
