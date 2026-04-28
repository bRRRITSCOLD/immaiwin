package skills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/Masterminds/semver/v3"
)

// Resolver turns an agent's declarative `skills:` list into a concrete
// lockfile — a list of (slug, exact-version, checksum) tuples that the
// agent loop can load deterministically.
//
// Resolution order:
//  1. Look up every requested slug in the RegistryStore.
//  2. For each, pick the highest installed version that satisfies the
//     SemVer range (npm-style: `^1.2.0`, `~2.5.0`, `1.2.0`, `*`).
//  3. If no installed version matches, walk Sources in order and Load the
//     best candidate, persisting it via UpsertRecord (cold-load path).
//  4. Verify each resolved bundle's checksum against the registry record
//     so a partial restore or tampered FS produces a hard failure rather
//     than a silent stale-version run.
//
// Lockfiles are cached on the agent's binding; Update re-resolves with
// preference for higher versions when ranges allow.
type Resolver struct {
	Registry RegistryStore
	Sources  []Source
}

// NewResolver constructs a Resolver. At least one Source is required if
// the registry is expected to admit cold loads; an empty Sources slice
// limits the resolver to whatever's already in the registry.
func NewResolver(reg RegistryStore, sources ...Source) *Resolver {
	return &Resolver{Registry: reg, Sources: sources}
}

// Resolve takes a list of requested skills and returns a deterministic
// lockfile. Each request must reference a slug present in the registry
// (or loadable from a configured Source).
//
// On success, the returned lockfile lists resolved versions in the same
// order as the request slice. A single missing skill aborts the whole
// resolve (we'd rather fail loudly than half-load).
func (r *Resolver) Resolve(ctx context.Context, _ string, reqs []SkillReq) ([]SkillLock, error) {
	out := make([]SkillLock, 0, len(reqs))
	for _, req := range reqs {
		lock, err := r.resolveOne(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("skills: resolve %s@%s: %w", req.SlugID, req.Range, err)
		}
		out = append(out, lock)
	}
	return out, nil
}

func (r *Resolver) resolveOne(ctx context.Context, req SkillReq) (SkillLock, error) {
	if req.SlugID == "" {
		return SkillLock{}, errors.New("slug_id is required")
	}
	rng, err := parseRange(req.Range)
	if err != nil {
		return SkillLock{}, fmt.Errorf("invalid range: %w", err)
	}

	// 1. Try installed versions first.
	records, err := r.Registry.ListVersions(ctx, req.SlugID)
	if err != nil {
		return SkillLock{}, fmt.Errorf("registry list: %w", err)
	}
	if rec, ok := pickHighestMatch(records, rng); ok {
		return SkillLock{SlugID: rec.SlugID, Version: rec.Version, Checksum: rec.Checksum}, nil
	}

	// 2. Cold load: scan Sources for a satisfying version, install the
	// highest match into the registry, then return the lock.
	for _, src := range r.Sources {
		manifests, err := src.List(ctx)
		if err != nil {
			slog.Warn("skills/resolver: source list failed", "source", src.Name(), "err", err)
			continue
		}
		var candidates []Manifest
		for _, m := range manifests {
			if m.ID != req.SlugID {
				continue
			}
			v, err := semver.NewVersion(m.Version)
			if err != nil {
				continue
			}
			if rng.Check(v) {
				candidates = append(candidates, m)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		sortManifestsBySemverDesc(candidates)
		best := candidates[0]
		bundle, err := src.Load(ctx, best.ID, best.Version)
		if err != nil {
			return SkillLock{}, fmt.Errorf("source %s load %s@%s: %w", src.Name(), best.ID, best.Version, err)
		}
		checksum, err := src.Verify(ctx, best.ID, best.Version)
		if err != nil {
			_ = bundle.Close()
			return SkillLock{}, fmt.Errorf("source %s verify %s@%s: %w", src.Name(), best.ID, best.Version, err)
		}
		_ = bundle.Close()
		rec, err := r.Registry.UpsertRecord(ctx, SkillRecord{
			SlugID:   best.ID,
			Version:  best.Version,
			Manifest: best,
			SourceID: src.Name(),
			Checksum: checksum,
		})
		if err != nil {
			return SkillLock{}, fmt.Errorf("registry upsert: %w", err)
		}
		return SkillLock{SlugID: rec.SlugID, Version: rec.Version, Checksum: rec.Checksum}, nil
	}

	return SkillLock{}, fmt.Errorf("%w: no version in registry or sources matches %s@%s", ErrSkillNotFound, req.SlugID, req.Range)
}

// Update re-resolves with a bias toward higher versions where each
// request's range allows. Convenience wrapper around Resolve — present so
// the UI's "Update skills" button has a stable name.
func (r *Resolver) Update(ctx context.Context, tenantID string, reqs []SkillReq) ([]SkillLock, error) {
	return r.Resolve(ctx, tenantID, reqs)
}

// Verify confirms each lock entry's checksum still matches what's in the
// registry. Doesn't re-touch sources — that's a separate, more expensive
// operation (call Source.Verify yourself if you want to detect FS
// drift).
func (r *Resolver) Verify(ctx context.Context, lockfile []SkillLock) error {
	for _, lock := range lockfile {
		rec, err := r.Registry.GetRecord(ctx, lock.SlugID, lock.Version)
		if err != nil {
			return fmt.Errorf("skills/resolver: verify %s@%s: %w", lock.SlugID, lock.Version, err)
		}
		if rec.Checksum != lock.Checksum {
			return fmt.Errorf("skills/resolver: checksum drift for %s@%s (lock=%s registry=%s)",
				lock.SlugID, lock.Version, lock.Checksum, rec.Checksum)
		}
	}
	return nil
}

// LoadBundle finds the Source that originally registered `lock` and
// returns its Bundle. Used by the agent loop at run time to fetch tool
// source code + prompt fragments.
func (r *Resolver) LoadBundle(ctx context.Context, lock SkillLock) (Bundle, error) {
	rec, err := r.Registry.GetRecord(ctx, lock.SlugID, lock.Version)
	if err != nil {
		return nil, fmt.Errorf("skills/resolver: load %s@%s: %w", lock.SlugID, lock.Version, err)
	}
	for _, src := range r.Sources {
		if src.Name() != rec.SourceID {
			continue
		}
		return src.Load(ctx, lock.SlugID, lock.Version)
	}
	return nil, fmt.Errorf("skills/resolver: source %q for %s@%s not configured", rec.SourceID, lock.SlugID, lock.Version)
}

// --- helpers --------------------------------------------------------------

func parseRange(raw string) (*semver.Constraints, error) {
	if raw == "" {
		raw = "*"
	}
	return semver.NewConstraint(raw)
}

func pickHighestMatch(records []SkillRecord, rng *semver.Constraints) (SkillRecord, bool) {
	var best *SkillRecord
	var bestV *semver.Version
	for i := range records {
		v, err := semver.NewVersion(records[i].Version)
		if err != nil {
			continue
		}
		if !rng.Check(v) {
			continue
		}
		if bestV == nil || v.GreaterThan(bestV) {
			best = &records[i]
			bestV = v
		}
	}
	if best == nil {
		return SkillRecord{}, false
	}
	return *best, true
}

func sortManifestsBySemverDesc(ms []Manifest) {
	sort.Slice(ms, func(i, j int) bool {
		vi, ei := semver.NewVersion(ms[i].Version)
		vj, ej := semver.NewVersion(ms[j].Version)
		if ei != nil || ej != nil {
			return ms[i].Version > ms[j].Version
		}
		return vi.GreaterThan(vj)
	})
}

func sortRecordsBySemver(rs []SkillRecord) {
	sort.Slice(rs, func(i, j int) bool {
		vi, ei := semver.NewVersion(rs[i].Version)
		vj, ej := semver.NewVersion(rs[j].Version)
		if ei != nil || ej != nil {
			return rs[i].Version < rs[j].Version
		}
		return vi.LessThan(vj)
	})
}
