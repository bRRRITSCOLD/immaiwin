package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// validPkgName allows alphanumerics, dash, underscore, dot, slash, at,
// equals, greater/less-than, caret, tilde, colon — covers npm, pip, go, cargo, composer.
var validPkgName = regexp.MustCompile(`^[a-zA-Z0-9\-_\./@=><^~:]+$`)

// ImageBuilder builds Docker images on-the-fly with user-specified packages.
type ImageBuilder struct {
	cli      client.APIClient
	registry string // optional prefix for tagged images
}

// NewImageBuilder creates a builder backed by the given Docker client.
// registry is an optional image-tag prefix (e.g. "localhost:5000"); empty = no prefix.
func NewImageBuilder(cli client.APIClient, registry string) *ImageBuilder {
	return &ImageBuilder{cli: cli, registry: registry}
}

// BuildOrReuse returns a tagged image that contains the requested packages.
// If the image already exists locally, it skips the build.
func (b *ImageBuilder) BuildOrReuse(ctx context.Context, lang sandbox.Language, base string, packages []string, debug bool) (string, error) {
	for _, pkg := range packages {
		if !validatePackageName(pkg) {
			return "", fmt.Errorf("sandbox/docker: invalid package name: %q", pkg)
		}
	}

	baseID, err := b.resolveBaseID(ctx, base)
	if err != nil {
		return "", fmt.Errorf("sandbox/docker: resolve base image %q: %w", base, err)
	}

	tag := buildTag(b.registry, base, baseID, packages)

	_, err = b.cli.ImageInspect(ctx, tag)
	if err == nil {
		slog.Info("sandbox/docker: reusing existing package image", "tag", tag)
		return tag, nil
	}

	slog.Info("sandbox/docker: building package image", "tag", tag, "base", base, "packages", packages)

	dockerfile := generateDockerfile(lang, base, packages)
	tarBuf, err := dockerfileTar(dockerfile)
	if err != nil {
		return "", fmt.Errorf("sandbox/docker: tar context: %w", err)
	}

	resp, err := b.cli.ImageBuild(ctx, tarBuf, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
		NoCache:    false,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox/docker: image build: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := parseBuildOutput(resp.Body); err != nil {
		return "", fmt.Errorf("sandbox/docker: build failed: %w", err)
	}

	slog.Info("sandbox/docker: package image built", "tag", tag)
	return tag, nil
}

// buildTag generates a deterministic image tag from the base image content
// digest + sorted packages. Hashing the digest (not just the name) means a
// rebuilt base image — same tag, new content — produces a new pkg tag and
// forces a rebuild instead of silently reusing a stale derived image.
func buildTag(registry, base, baseID string, packages []string) string {
	sorted := make([]string, len(packages))
	copy(sorted, packages)
	sort.Strings(sorted)

	h := sha256.New()
	h.Write([]byte(base))
	h.Write([]byte{0})
	h.Write([]byte(baseID))
	h.Write([]byte{0})
	for _, pkg := range sorted {
		h.Write([]byte(pkg))
		h.Write([]byte{0})
	}
	hash := fmt.Sprintf("%x", h.Sum(nil))[:16]

	tag := fmt.Sprintf("immaiwin/sandbox-pkg:%s", hash)
	if registry != "" {
		tag = registry + "/" + tag
	}
	return tag
}

// resolveBaseID returns the content ID (sha256 of image config) for base.
// Pulls the image first if not present locally.
func (b *ImageBuilder) resolveBaseID(ctx context.Context, base string) (string, error) {
	insp, err := b.cli.ImageInspect(ctx, base)
	if err == nil {
		return insp.ID, nil
	}

	slog.Info("sandbox/docker: pulling base image to resolve digest", "image", base)
	rc, perr := b.cli.ImagePull(ctx, base, image.PullOptions{})
	if perr != nil {
		return "", fmt.Errorf("pull: %w", perr)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return "", fmt.Errorf("drain pull stream: %w", err)
	}

	insp, err = b.cli.ImageInspect(ctx, base)
	if err != nil {
		return "", fmt.Errorf("inspect after pull: %w", err)
	}
	return insp.ID, nil
}

// generateDockerfile creates an in-memory Dockerfile that installs packages on top of the base.
// Switches to root for install (base images use non-root "sandbox" user), then restores.
func generateDockerfile(lang sandbox.Language, base string, packages []string) string {
	cmd := installCommand(lang, packages)
	return fmt.Sprintf("FROM %s\nUSER root\nRUN %s\nUSER sandbox\n", base, cmd)
}

// installCommand returns the shell command to install packages for the given language.
func installCommand(lang sandbox.Language, packages []string) string {
	joined := strings.Join(packages, " ")
	switch lang {
	case sandbox.LangJavaScript:
		return fmt.Sprintf("cd /sandbox && npm install %s", joined)
	case sandbox.LangPython:
		return fmt.Sprintf("pip install --no-cache-dir %s", joined)
	case sandbox.LangGolang:
		parts := []string{"cd /tmp", "go mod init tmp"}
		for _, pkg := range packages {
			p := pkg
			if !strings.Contains(p, "@") {
				p += "@latest"
			}
			parts = append(parts, fmt.Sprintf("go get %s", p))
		}
		parts = append(parts, "rm -rf /tmp/go.mod /tmp/go.sum")
		return strings.Join(parts, " && ")
	case sandbox.LangRust:
		parts := []string{"cd /tmp", "cargo init --name tmp_build"}
		for _, pkg := range packages {
			parts = append(parts, fmt.Sprintf("cargo add %s", pkg))
		}
		parts = append(parts, "cargo build", "rm -rf /tmp/tmp_build")
		return strings.Join(parts, " && ")
	case sandbox.LangPHP:
		return fmt.Sprintf("composer global require %s", joined)
	default:
		return fmt.Sprintf("echo 'unknown language: %s'", string(lang))
	}
}

// validatePackageName checks a package name against the allowlist.
func validatePackageName(name string) bool {
	return name != "" && validPkgName.MatchString(name)
}

// dockerfileTar creates an in-memory tar archive containing a single Dockerfile.
func dockerfileTar(dockerfile string) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)

	content := []byte(dockerfile)
	hdr := &tar.Header{
		Name: "Dockerfile",
		Mode: 0644,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}

// parseBuildOutput reads the Docker build JSON stream and returns an error if the build failed.
func parseBuildOutput(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("%s", msg.Error)
		}
		if msg.Stream != "" {
			slog.Debug("sandbox/docker: build", "output", strings.TrimSpace(msg.Stream))
		}
	}
}
