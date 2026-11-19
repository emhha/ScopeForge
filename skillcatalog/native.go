package skillcatalog

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	nativeskill "trpc.group/trpc-go/trpc-agent-go/skill"
)

// Repository returns a frozen knowledge-only view of this catalog's visible
// skills. It deliberately implements neither RefreshableRepository nor Roots:
// callers cannot rescan hidden skills or obtain the unfiltered repository root.
// Path is only a validated locator, never permission to stage/execute scripts.
func (c *Catalog) Repository() nativeskill.Repository {
	return &knowledgeRepository{catalog: c.Filter(allVisible(c))}
}

func allVisible(c *Catalog) map[string]bool {
	names := make(map[string]bool)
	for _, entry := range c.List() {
		names[entry.Name] = true
	}
	return names
}

type knowledgeRepository struct{ catalog *Catalog }

func (r *knowledgeRepository) Summaries() []nativeskill.Summary {
	out := make([]nativeskill.Summary, 0, len(r.catalog.entries))
	for _, entry := range r.catalog.entries {
		out = append(out, nativeskill.Summary{Name: entry.Name, Description: entry.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (r *knowledgeRepository) Get(name string) (*nativeskill.Skill, error) {
	entry, ok := r.catalog.Get(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	item, ok := r.catalog.native[entry.Name]
	if !ok {
		return nil, os.ErrNotExist
	}
	copy := cloneNative(item)
	return &copy, nil
}
func (r *knowledgeRepository) Path(name string) (string, error) {
	entry, ok := r.catalog.Get(name)
	if !ok {
		return "", os.ErrNotExist
	}
	if err := noSymlinkPath(r.catalog.root, entry.Dir); err != nil {
		return "", err
	}
	// A locator is not a lease. Revalidate even though Get reads only frozen
	// knowledge, so a post-load symlink never becomes an advertised skill path.
	root, err := os.OpenRoot(entry.Dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill locator contains symlink: %s", path)
		}
		return nil
	}); err != nil {
		return "", err
	}
	return entry.Dir, nil
}
func cloneNative(item nativeskill.Skill) nativeskill.Skill {
	item.Docs = append([]nativeskill.Doc(nil), item.Docs...)
	return item
}
func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// Product-only metadata. The SDK remains authoritative for name, description,
// Markdown body separation and auxiliary document discovery. In particular its
// fixed-version handling of quoted simple name/description scalars is preserved.
type productMetadata struct {
	License       string    `yaml:"license"`
	Compatibility string    `yaml:"compatibility"`
	MCPs          yaml.Node `yaml:"mcps"`
}

func readProductMetadata(data []byte) (productMetadata, []string, error) {
	var meta productMetadata
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return meta, nil, nil
	}
	// Decode the first YAML document, not a hand-written frontmatter parser.
	// yaml.Decoder stops at the next '---'; the SDK owns Markdown separation.
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&meta); err != nil {
		return meta, nil, err
	}
	var raw []string
	switch meta.MCPs.Kind {
	case 0:
	case yaml.ScalarNode:
		var value string
		if err := meta.MCPs.Decode(&value); err != nil {
			return meta, nil, err
		}
		raw = strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' })
	case yaml.SequenceNode:
		if err := meta.MCPs.Decode(&raw); err != nil {
			return meta, nil, err
		}
	default:
		return meta, nil, fmt.Errorf("skill mcps must be a string or string list")
	}
	var servers []string
	seen := make(map[string]bool)
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			servers = append(servers, name)
			seen[name] = true
		}
	}
	return meta, servers, nil
}

func loadNativeCatalog(root string) (*Catalog, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := noSymlinkPath(absolute, absolute); err != nil {
		return nil, err
	}
	base, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	var dirs []string
	err = fs.WalkDir(base.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill repository contains symlink: %s", path)
		}
		if !d.IsDir() && d.Name() == nativeskill.SkillFile {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Native FSRepository recursively reads docs. Nested skill roots would make
	// a visible parent disclose a hidden child's docs, so reject that layout.
	for _, dir := range dirs {
		for _, other := range dirs {
			if dir != other && (dir == "." || strings.HasPrefix(other, dir+string(filepath.Separator))) {
				return nil, fmt.Errorf("nested skill directories are not supported: %s", other)
			}
		}
	}
	catalog := &Catalog{root: absolute, native: make(map[string]nativeskill.Skill)}
	identities := make(map[string]string)
	for _, dir := range dirs {
		entry, item, err := loadNativeEntry(base, absolute, dir)
		if err != nil {
			return nil, err
		}
		for _, identity := range []string{entry.Name, filepath.Base(entry.Dir)} {
			if !safeName(identity) {
				return nil, fmt.Errorf("invalid skill identity %q", identity)
			}
			if previous, ok := identities[identity]; ok && previous != dir {
				return nil, fmt.Errorf("ambiguous skill name or directory alias %q", identity)
			}
			identities[identity] = dir
		}
		catalog.entries = append(catalog.entries, entry)
		catalog.native[entry.Name] = item
	}
	return catalog, nil
}

func loadNativeEntry(base *os.Root, absolute, dir string) (Entry, nativeskill.Skill, error) {
	var entry Entry
	var item nativeskill.Skill
	info, err := base.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return entry, item, fmt.Errorf("unsafe skill directory %s", dir)
	}
	source, err := base.OpenRoot(dir)
	if err != nil {
		return entry, item, err
	}
	defer source.Close()
	opened, err := source.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return entry, item, fmt.Errorf("skill directory changed during load")
	}
	stage, err := os.MkdirTemp("", "scopeforge-skill-knowledge-")
	if err != nil {
		return entry, item, err
	}
	defer os.RemoveAll(stage) // exact path returned by MkdirTemp, never a caller path
	// Keep the original directory basename for the SDK's fallback skill name.
	target := filepath.Join(stage, filepath.Base(filepath.Join(absolute, dir)))
	if err := os.Mkdir(target, 0700); err != nil {
		return entry, item, err
	}
	var definition []byte
	err = fs.WalkDir(source.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill content contains symlink: %s", path)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("skill content is not a regular file: %s", path)
		}
		// Copy knowledge documents only, never scripts/assets/the entire tree.
		ext := strings.ToLower(filepath.Ext(path))
		if d.Name() != nativeskill.SkillFile && ext != ".md" && ext != ".txt" {
			return nil
		}
		data, err := readRegular(source, path)
		if err != nil {
			return err
		}
		if path == nativeskill.SkillFile {
			definition = data
		}
		destination := filepath.Join(target, path)
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0600)
	})
	if err != nil {
		return entry, item, err
	}
	meta, servers, err := readProductMetadata(definition)
	if err != nil {
		return entry, item, fmt.Errorf("skill %s metadata: %w", dir, err)
	}
	repo, err := nativeskill.NewFSRepository(target)
	if err != nil {
		return entry, item, err
	}
	summaries := repo.Summaries()
	if len(summaries) != 1 {
		return entry, item, fmt.Errorf("skill %s must have one native definition", dir)
	}
	loaded, err := repo.Get(summaries[0].Name)
	if err != nil {
		return entry, item, err
	}
	item = cloneNative(*loaded)
	entry = Entry{Skill: Skill{Name: item.Summary.Name, Description: item.Summary.Description,
		License: meta.License, Compatibility: meta.Compatibility,
		Path: filepath.Join(absolute, dir, nativeskill.SkillFile), Body: item.Body},
		Dir: filepath.Join(absolute, dir), MCPs: servers}
	return entry, item, nil
}

// OpenRoot confines all reads to this one skill, not the catalog (which could
// contain hidden siblings). SameFile rejects a file swapped between Lstat/Open.
func readRegular(root *os.Root, path string) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("unsafe skill document %s", path)
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("skill document changed during load")
	}
	return io.ReadAll(file)
}

func noSymlinkPath(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("skill path escapes repository")
	}
	// Include every ancestor of root: OpenRoot itself otherwise follows a
	// symlink supplied as the root, unlike confined operations inside it.
	current := filepath.VolumeName(path) + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe skill directory %s", current)
		}
	}
	return nil
}
