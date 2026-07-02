// Package config captures the configuration-function calls from BLADE_ROOT and
// blade.conf (cc_config, proto_library_config, cc_toolchain_config, ...).
//
// The loader records each call verbatim; interpretation (merge semantics,
// deferred lambda evaluation) is left to later phases. Attribute values may
// include Starlark callables (the `lambda blade: ...` deferred-config idiom),
// stored as-is for evaluation once the build-phase `blade` context exists.
package config

// Section is one configuration-function call and its keyword arguments.
type Section struct {
	Name  string         // "cc_config", "cc_toolchain_config", ...
	Attrs map[string]any // keyword args (Starlark converted; may hold callables)
	Pos   string         // source location
}

// Config accumulates configuration calls in call order.
type Config struct {
	sections []Section
}

// New returns an empty Config.
func New() *Config { return &Config{} }

// Record appends a configuration call.
func (c *Config) Record(name string, attrs map[string]any, pos string) {
	c.sections = append(c.sections, Section{Name: name, Attrs: attrs, Pos: pos})
}

// Sections returns all recorded calls in order.
func (c *Config) Sections() []Section { return c.sections }

// Named returns the calls with the given function name (some, like
// cc_toolchain_config, are called more than once).
func (c *Config) Named(name string) []Section {
	var out []Section
	for _, s := range c.sections {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}
