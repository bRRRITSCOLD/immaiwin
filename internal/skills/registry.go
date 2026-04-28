package skills

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RegistryStore persists the platform-wide library of installed skill
// versions. Implementations live in `internal/mongodb` (Mongo-backed) and
// `internal/skills` (in-memory, used by tests + small deploys).
//
// The store is the *index* — it records which (slug, version, source,
// checksum) tuples exist. Bundle bytes live behind a Source; the store
// just tells the Resolver where to look.
type RegistryStore interface {
	// UpsertRecord installs (or refreshes) a skill version in the registry.
	// Returns the persisted record so callers can pick up server-set fields
	// (e.g. ULID, InstalledAt).
	UpsertRecord(ctx context.Context, rec SkillRecord) (SkillRecord, error)

	// GetRecord fetches a (slug, version) pair. Returns ErrSkillNotFound
	// if missing.
	GetRecord(ctx context.Context, slugID, version string) (SkillRecord, error)

	// ListVersions returns every installed version for a slug, sorted
	// ascending by SemVer (resolver picks the highest match).
	ListVersions(ctx context.Context, slugID string) ([]SkillRecord, error)

	// ListAll returns every record in the registry. Used by the skill
	// library UI.
	ListAll(ctx context.Context) ([]SkillRecord, error)

	// DeleteRecord removes a (slug, version) entry.
	DeleteRecord(ctx context.Context, slugID, version string) error
}

// MemoryRegistry is an in-process RegistryStore. Use for tests or small
// single-node deployments. Loses state on restart; Mongo-backed store is
// canonical for prod.
type MemoryRegistry struct {
	mu      sync.RWMutex
	records map[string]SkillRecord // key: "slug@version"
	idSeq   int64
}

// NewMemoryRegistry constructs an empty in-memory registry.
func NewMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{records: make(map[string]SkillRecord)}
}

// UpsertRecord implements RegistryStore.
func (r *MemoryRegistry) UpsertRecord(_ context.Context, rec SkillRecord) (SkillRecord, error) {
	if rec.SlugID == "" || rec.Version == "" {
		return SkillRecord{}, errors.New("skills/memory: slug_id and version are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.ID == "" {
		r.idSeq++
		rec.ID = newRecordID(r.idSeq)
	}
	if rec.InstalledAt.IsZero() {
		rec.InstalledAt = time.Now().UTC()
	}
	r.records[rec.SlugID+"@"+rec.Version] = rec
	return rec, nil
}

// GetRecord implements RegistryStore.
func (r *MemoryRegistry) GetRecord(_ context.Context, slugID, version string) (SkillRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if rec, ok := r.records[slugID+"@"+version]; ok {
		return rec, nil
	}
	return SkillRecord{}, ErrSkillNotFound
}

// ListVersions implements RegistryStore.
func (r *MemoryRegistry) ListVersions(_ context.Context, slugID string) ([]SkillRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []SkillRecord
	for _, rec := range r.records {
		if rec.SlugID == slugID {
			out = append(out, rec)
		}
	}
	sortRecordsBySemver(out)
	return out, nil
}

// ListAll implements RegistryStore.
func (r *MemoryRegistry) ListAll(_ context.Context) ([]SkillRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SkillRecord, 0, len(r.records))
	for _, rec := range r.records {
		out = append(out, rec)
	}
	sortRecordsBySemver(out)
	return out, nil
}

// DeleteRecord implements RegistryStore.
func (r *MemoryRegistry) DeleteRecord(_ context.Context, slugID, version string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, slugID+"@"+version)
	return nil
}

// newRecordID is a deterministic-but-unique ID for the in-memory store.
// Mongo-backed store will use ULID for global ordering.
func newRecordID(seq int64) string {
	return "memreg-" + formatInt(seq)
}

// formatInt avoids strconv import for one tiny callsite.
func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
