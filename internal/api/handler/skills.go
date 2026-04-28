// Package handler — skills HTTP surface.
//
// Routes:
//   GET  /api/v1/skills           — list every installed skill version (registry contents)
//   POST /api/v1/skills/refresh   — scan all configured Sources, install any new
//                                    (slug, version) pairs into the registry
//
// More routes (install / uninstall / per-tenant install gating) land with
// P1.12+ multi-tenancy. The current shape is enough to power the agent
// node's "Add skill" picker.

package handler

import (
	"context"
	"net/http"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/skills"
	"github.com/gin-gonic/gin"
)

// SkillBackend bundles the registry + sources the API needs to expose.
// Threaded into the server constructor so tests can stub.
type SkillBackend struct {
	Registry skills.RegistryStore
	Sources  []skills.Source
}

// ListSkills returns every record in the registry. Empty list when the
// skills system is disabled or the registry is empty.
func ListSkills(b *SkillBackend) gin.HandlerFunc {
	return func(c *gin.Context) {
		if b == nil || b.Registry == nil {
			c.JSON(http.StatusOK, []skills.SkillRecord{})
			return
		}
		records, err := b.Registry.ListAll(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if records == nil {
			records = []skills.SkillRecord{}
		}
		c.JSON(http.StatusOK, records)
	}
}

// RefreshSkills walks every configured Source, parses each manifest, and
// upserts the corresponding (slug, version) pair into the registry.
// Idempotent — calling twice is a no-op aside from refreshed checksums.
func RefreshSkills(b *SkillBackend) gin.HandlerFunc {
	return func(c *gin.Context) {
		if b == nil || b.Registry == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "skills system disabled"})
			return
		}
		ctx := c.Request.Context()

		var imported int
		var errs []string

		for _, src := range b.Sources {
			n, err := refreshSource(ctx, b.Registry, src)
			imported += n
			if err != nil {
				errs = append(errs, src.Name()+": "+err.Error())
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"imported": imported,
			"errors":   errs,
		})
	}
}

func refreshSource(ctx context.Context, reg skills.RegistryStore, src skills.Source) (int, error) {
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
		if _, err := reg.UpsertRecord(ctx, skills.SkillRecord{
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
