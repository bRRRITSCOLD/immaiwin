package skills

import (
	"context"
	"errors"
	"io"
)

// Source is the abstraction over "where do skill bundles live."
//
// A Source can be a directory on disk (LocalFS), a Mongo GridFS bucket, an
// S3 prefix, an OCI registry, etc. The interface is intentionally narrow —
// list available manifests, load a specific (slug, version) bundle, and
// verify its checksum — so out-of-tree sources stay cheap to implement.
//
// Multiple Sources can be active simultaneously; the Resolver consults them
// in registration order until one yields a matching version.
type Source interface {
	// Name returns the source's identifier (e.g. "local-fs", "mongo", "s3").
	// Used in log lines and as the value persisted in
	// `SkillRecord.SourceID`.
	Name() string

	// List discovers every manifest reachable from this source. Cheap-to-
	// scan sources (LocalFS) implement this directly; heavier sources
	// (HTTPS, OCI) may cache.
	List(ctx context.Context) ([]Manifest, error)

	// Load fetches a specific skill version. The returned Bundle exposes
	// the manifest plus file-content read access scoped to the bundle.
	Load(ctx context.Context, slugID, version string) (Bundle, error)

	// Verify recomputes the bundle checksum and returns it so callers can
	// confirm a registry entry hasn't drifted from the on-disk content.
	Verify(ctx context.Context, slugID, version string) (string, error)
}

// Bundle is a loaded skill bundle: manifest + on-demand file access.
//
// Lifetime tied to the loading Source — Close releases any handles
// (filesystem walkers, memory-mapped tarballs, GridFS streams) opened
// during Load. Callers must always Close.
type Bundle interface {
	Manifest() Manifest

	// Open returns a reader for a relative path inside the bundle (e.g. a
	// tool's `file:` value). Errors if the path escapes the bundle root or
	// doesn't exist.
	Open(path string) (io.ReadCloser, error)

	// ReadString is a convenience for the common case of slurping a tool's
	// source code as a string before handing it to the sandbox runtime.
	ReadString(path string) (string, error)

	// Files returns every relative path in the bundle. Useful for
	// integrity checks and skill-library UIs.
	Files() []string

	Close() error
}

// ErrSkillNotFound is returned by Source.Load when the requested
// (slug, version) pair isn't reachable. Callers can errors.Is against this
// to distinguish "not present" from operational failures.
var ErrSkillNotFound = errors.New("skills: skill not found")
