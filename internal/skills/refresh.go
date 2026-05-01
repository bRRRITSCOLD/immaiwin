// Refresh helper — walks every Source's manifests and upserts each
// (slug, version) into the registry. Shared by the HTTP handler
// (`POST /api/v1/skills/refresh`) and the API boot path so a fresh
// process auto-imports bundled skills without the operator having to
// click "Refresh" by hand.
//
// Idempotent: subsequent calls just refresh checksums.

package skills

import (
	"context"
	"fmt"
)

// RefreshResult bundles per-call counters so callers can log a single
// summary line. Errors is one entry per failing source — a partial
// failure (one source good, one bad) returns counts AND errors.
type RefreshResult struct {
	Imported int
	Errors   []string
}

// RefreshFromSources scans every source, upserts each (slug, version)
// pair into the registry, and returns aggregate counts. Per-source
// failures don't abort the whole walk; they're collected into
// `Errors`. Per-manifest failures (e.g. a single bundle's checksum
// computation blew up) are silently skipped — the warn is the
// downstream caller's responsibility, not this helper's.
func RefreshFromSources(ctx context.Context, reg RegistryStore, sources []Source) RefreshResult {
	var result RefreshResult
	if reg == nil {
		return result
	}
	for _, src := range sources {
		n, err := refreshOneSource(ctx, reg, src)
		result.Imported += n
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", src.Name(), err))
		}
	}
	return result
}

func refreshOneSource(ctx context.Context, reg RegistryStore, src Source) (int, error) {
	manifests, err := src.List(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, m := range manifests {
		checksum, err := src.Verify(ctx, m.ID, m.Version)
		if err != nil {
			continue
		}
		if _, err := reg.UpsertRecord(ctx, SkillRecord{
			SlugID:   m.ID,
			Version:  m.Version,
			Manifest: m,
			SourceID: src.Name(),
			Checksum: checksum,
		}); err != nil {
			continue
		}
		n++
	}
	return n, nil
}
