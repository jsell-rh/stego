package registry

import "github.com/jsell-rh/stego/internal/parser"

// ParseArchetype reads and parses an archetype YAML file.
func ParseArchetype(path string) (*Archetype, error) {
	return parser.ReadKind(path, "archetype", archetypeKind)
}

// ParseComponent reads and parses a component YAML file.
func ParseComponent(path string) (*Component, error) {
	return parser.ReadKind(path, "component", componentKind)
}

// ParseMixin reads and parses a mixin YAML file.
func ParseMixin(path string) (*Mixin, error) {
	return parser.ReadKind(path, "mixin", mixinKind)
}

// ParseArchetypeFromBytes parses one captured registry document.
func ParseArchetypeFromBytes(data []byte, path string) (*Archetype, error) {
	return parser.ParseKind(data, path, "archetype", archetypeKind)
}

// ParseComponentFromBytes parses one captured registry document.
func ParseComponentFromBytes(data []byte, path string) (*Component, error) {
	return parser.ParseKind(data, path, "component", componentKind)
}

// ParseMixinFromBytes parses one captured registry document.
func ParseMixinFromBytes(data []byte, path string) (*Mixin, error) {
	return parser.ParseKind(data, path, "mixin", mixinKind)
}

func archetypeKind(a *Archetype) string { return a.Kind }

func componentKind(c *Component) string { return c.Kind }

func mixinKind(m *Mixin) string { return m.Kind }
