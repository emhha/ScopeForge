// Package skills supplies product visibility and metadata over the pinned native
// skill repository. It does not execute skill scripts or implement an agent loop.
package skillcatalog

import (
	"path/filepath"

	nativeskill "trpc.group/trpc-go/trpc-agent-go/skill"
)

type Skill struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	Path          string
	Body          string
}

type Entry struct {
	Skill
	Dir  string
	MCPs []string
}

type Catalog struct {
	entries  []Entry
	native   map[string]nativeskill.Skill
	root     string
	OnInvoke func(Entry) string
}

// Load delegates body/doc discovery and reading to the native FSRepository over
// a private, symlink-free snapshot. Product metadata uses yaml.v3, not a second
// hand-written YAML parser. The returned catalog owns no temporary files.
func Load(root string) (*Catalog, error) { return loadNativeCatalog(root) }

func cloneEntry(entry Entry) Entry {
	entry.MCPs = append([]string(nil), entry.MCPs...)
	return entry
}

func (c *Catalog) List() []Entry {
	if c == nil {
		return nil
	}
	out := make([]Entry, 0, len(c.entries))
	for _, entry := range c.entries {
		out = append(out, cloneEntry(entry))
	}
	return out
}
func (c *Catalog) Get(name string) (Entry, bool) {
	if c == nil || !safeName(name) {
		return Entry{}, false
	}
	for _, entry := range c.entries {
		if entry.Name == name || filepath.Base(entry.Dir) == name {
			return cloneEntry(entry), true
		}
	}
	return Entry{}, false
}
func (c *Catalog) Filter(names map[string]bool) *Catalog {
	filtered := &Catalog{native: make(map[string]nativeskill.Skill)}
	if c == nil {
		return filtered
	}
	filtered.root = c.root
	for _, entry := range c.entries {
		if names[filepath.Base(entry.Dir)] || names[entry.Name] {
			filtered.entries = append(filtered.entries, cloneEntry(entry))
			if item, ok := c.native[entry.Name]; ok {
				filtered.native[entry.Name] = cloneNative(item)
			}
		}
	}
	return filtered
}
