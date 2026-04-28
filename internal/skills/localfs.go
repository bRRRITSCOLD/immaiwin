package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// LocalFSSource serves skill bundles from a directory tree on disk.
//
// Layout (matches SKILLS-AND-PLUGINS-PLAN.md §1.1):
//
//	$root/
//	├── weather-pro/
//	│   ├── manifest.yaml
//	│   ├── tools/...
//	│   └── prompts/...
//	└── data-utils/
//	    └── ...
//
// The directory name is treated as the slug ID. Versioning is encoded in
// the manifest, not the path — so multiple installed versions of the same
// slug share one directory only if a Mongo-backed source is layered above
// (LocalFS exposes one version per slug at a time, which mirrors how a
// developer-checkout tree typically looks). When a registry needs multiple
// versions of the same skill, mount each version under a versioned directory
// and let the manifest's `version` field disambiguate.
type LocalFSSource struct {
	root string
	name string

	mu    sync.RWMutex
	cache map[string]localBundleEntry // keyed by "slug@version"
}

type localBundleEntry struct {
	manifest Manifest
	dir      string
	files    []string
	checksum string
}

// NewLocalFSSource constructs a LocalFS-backed Source rooted at `root`.
// `name` overrides the default "local-fs" identifier (handy when multiple
// LocalFS sources are configured, e.g. "bundled" + "tenant-skills").
func NewLocalFSSource(root, name string) *LocalFSSource {
	if name == "" {
		name = "local-fs"
	}
	return &LocalFSSource{
		root:  root,
		name:  name,
		cache: make(map[string]localBundleEntry),
	}
}

// Name implements Source.
func (l *LocalFSSource) Name() string { return l.name }

// List implements Source. Walks `root` one level deep — each immediate
// subdirectory containing a `manifest.yaml` is treated as a candidate
// bundle. Invalid manifests are skipped with a logged error rather than
// failing the whole scan; one bad bundle shouldn't hide the rest.
func (l *LocalFSSource) List(ctx context.Context) ([]Manifest, error) {
	entries, err := os.ReadDir(l.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // empty source is valid
		}
		return nil, fmt.Errorf("skills/localfs: read root %q: %w", l.root, err)
	}

	var out []Manifest
	for _, ent := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !ent.IsDir() {
			continue
		}
		manifestPath := filepath.Join(l.root, ent.Name(), ManifestFileName)
		if _, err := os.Stat(manifestPath); err != nil {
			continue
		}
		m, err := LoadManifestFile(manifestPath)
		if err != nil {
			// Soft-fail: log and continue scanning. Hard failure would let
			// a single broken skill block library discovery.
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].Version < out[j].Version
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Load implements Source. Caches the parsed manifest + file index per
// (slug, version). On cache miss, scans the matching directory, validates
// the manifest, and computes a sha256-over-files checksum.
func (l *LocalFSSource) Load(ctx context.Context, slugID, version string) (Bundle, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	key := slugID + "@" + version

	l.mu.RLock()
	if entry, ok := l.cache[key]; ok {
		l.mu.RUnlock()
		return &localBundle{src: l, entry: entry}, nil
	}
	l.mu.RUnlock()

	dir := filepath.Join(l.root, dirNameForSlug(slugID))
	manifestPath := filepath.Join(dir, ManifestFileName)
	if _, err := os.Stat(manifestPath); err != nil {
		return nil, fmt.Errorf("%w: %s@%s", ErrSkillNotFound, slugID, version)
	}
	m, err := LoadManifestFile(manifestPath)
	if err != nil {
		return nil, err
	}
	if m.ID != slugID {
		return nil, fmt.Errorf("skills/localfs: manifest id %q does not match directory slug %q", m.ID, slugID)
	}
	if version != "" && m.Version != version {
		return nil, fmt.Errorf("%w: %s@%s (found %s)", ErrSkillNotFound, slugID, version, m.Version)
	}

	files, err := walkBundleFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("skills/localfs: walk bundle %q: %w", dir, err)
	}
	checksum, err := hashBundle(dir, files)
	if err != nil {
		return nil, fmt.Errorf("skills/localfs: hash bundle %q: %w", dir, err)
	}

	entry := localBundleEntry{
		manifest: m,
		dir:      dir,
		files:    files,
		checksum: checksum,
	}
	l.mu.Lock()
	l.cache[slugID+"@"+m.Version] = entry
	l.mu.Unlock()

	return &localBundle{src: l, entry: entry}, nil
}

// Verify implements Source by re-running the bundle hash without consulting
// the cache. Used to catch tampering or a partial restore.
func (l *LocalFSSource) Verify(ctx context.Context, slugID, version string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	dir := filepath.Join(l.root, dirNameForSlug(slugID))
	files, err := walkBundleFiles(dir)
	if err != nil {
		return "", err
	}
	return hashBundle(dir, files)
}

// Invalidate drops a cached bundle entry. Useful for fsnotify-driven hot
// reload; in P1 we don't wire the watcher, but the surface is here.
func (l *LocalFSSource) Invalidate(slugID, version string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.cache, slugID+"@"+version)
}

// dirNameForSlug returns the on-disk directory name for a slug. Namespaced
// slugs (`acme/weather-pro`) map to a subdirectory tree; the manifest itself
// stays the source of truth for ID, so this only affects layout.
func dirNameForSlug(slugID string) string {
	// On-disk we collapse `/` to `--` so a single ReadDir level stays cheap
	// to scan. A future LocalFS source can opt into nested layout if it
	// matters for org-style namespaces.
	return strings.ReplaceAll(slugID, "/", "--")
}

// walkBundleFiles returns every regular-file relative path inside dir,
// sorted for deterministic checksum input.
func walkBundleFiles(dir string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(rels)
	return rels, nil
}

// hashBundle returns sha256-hex over (sorted file paths || file contents).
// Matches the verify-checksum-after-restore workflow Resolver uses.
func hashBundle(dir string, files []string) (string, error) {
	h := sha256.New()
	for _, rel := range files {
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(h, f)
		_ = f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// --- Bundle implementation -----------------------------------------------

type localBundle struct {
	src   *LocalFSSource
	entry localBundleEntry
}

func (b *localBundle) Manifest() Manifest { return b.entry.manifest }

func (b *localBundle) Open(path string) (io.ReadCloser, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return nil, fmt.Errorf("skills/localfs: refused unsafe bundle path %q", path)
	}
	full := filepath.Join(b.entry.dir, clean)
	// Defense-in-depth: refuse to open anything outside dir.
	abs, err := filepath.Abs(full)
	if err != nil {
		return nil, err
	}
	rootAbs, err := filepath.Abs(b.entry.dir)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(abs, rootAbs) {
		return nil, fmt.Errorf("skills/localfs: refused path escape %q", path)
	}
	return os.Open(full)
}

func (b *localBundle) ReadString(path string) (string, error) {
	rc, err := b.Open(path)
	if err != nil {
		return "", err
	}
	defer rc.Close() //nolint:errcheck
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (b *localBundle) Files() []string {
	out := make([]string, len(b.entry.files))
	copy(out, b.entry.files)
	return out
}

func (b *localBundle) Close() error { return nil }
