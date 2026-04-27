package sandbox

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

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// validPkgName allows alphanumerics, dash, underscore, dot, slash, at,
// equals, greater/less-than, caret, tilde, colon — covers npm, pip, go, cargo, composer.
var validPkgName = regexp.MustCompile(`^[a-zA-Z0-9\-_\./@=><^~:]+$`)

// ImageBuilder builds Docker images on-the-fly with user-specified packages.
type ImageBuilder struct {
	cli client.APIClient
}

// NewImageBuilder creates a builder backed by the given Docker client.
func NewImageBuilder(cli client.APIClient) *ImageBuilder {
	return &ImageBuilder{cli: cli}
}

// BuildOrReuse returns a tagged image that contains the requested packages.
// If the image already exists locally, it skips the build.
func (b *ImageBuilder) BuildOrReuse(ctx context.Context, lang Language, base string, packages []string, debug bool) (string, error) {
	for _, pkg := range packages {
		if !validatePackageName(pkg) {
			return "", fmt.Errorf("sandbox: invalid package name: %q", pkg)
		}
	}

	tag := buildTag(base, packages)

	// Check if image exists
	_, _, err := b.cli.ImageInspectWithRaw(ctx, tag)
	if err == nil {
		slog.Info("sandbox: reusing existing package image", "tag", tag)
		return tag, nil
	}

	slog.Info("sandbox: building package image", "tag", tag, "base", base, "packages", packages)

	dockerfile := generateDockerfile(lang, base, packages)
	tarBuf, err := dockerfileTar(dockerfile)
	if err != nil {
		return "", fmt.Errorf("sandbox: tar context: %w", err)
	}

	resp, err := b.cli.ImageBuild(ctx, tarBuf, types.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
		NoCache:    false,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: image build: %w", err)
	}
	defer resp.Body.Close()

	// Parse build output for errors
	if err := parseBuildOutput(resp.Body); err != nil {
		return "", fmt.Errorf("sandbox: build failed: %w", err)
	}

	slog.Info("sandbox: package image built", "tag", tag)
	return tag, nil
}

// buildTag generates a deterministic image tag from the base image + sorted packages.
// Same packages → same tag → no rebuild.
func buildTag(base string, packages []string) string {
	sorted := make([]string, len(packages))
	copy(sorted, packages)
	sort.Strings(sorted)

	h := sha256.New()
	h.Write([]byte(base))
	h.Write([]byte{0})
	for _, pkg := range sorted {
		h.Write([]byte(pkg))
		h.Write([]byte{0})
	}
	hash := fmt.Sprintf("%x", h.Sum(nil))[:16]
	return fmt.Sprintf("immaiwin/sandbox-pkg:%s", hash)
}

// generateDockerfile creates an in-memory Dockerfile that installs packages on top of the base.
// Switches to root for install (base images use non-root "sandbox" user), then restores.
func generateDockerfile(lang Language, base string, packages []string) string {
	cmd := installCommand(lang, packages)
	return fmt.Sprintf("FROM %s\nUSER root\nRUN %s\nUSER sandbox\n", base, cmd)
}

// installCommand returns the shell command to install packages for the given language.
func installCommand(lang Language, packages []string) string {
	joined := strings.Join(packages, " ")
	switch lang {
	case LangJavaScript:
		return fmt.Sprintf("cd /sandbox && npm install %s", joined)
	case LangPython:
		return fmt.Sprintf("pip install --no-cache-dir %s", joined)
	case LangGolang:
		// Go packages need a module context
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
	case LangRust:
		parts := []string{"cd /tmp", "cargo init --name tmp_build"}
		for _, pkg := range packages {
			parts = append(parts, fmt.Sprintf("cargo add %s", pkg))
		}
		parts = append(parts, "cargo build", "rm -rf /tmp/tmp_build")
		return strings.Join(parts, " && ")
	case LangPHP:
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
			slog.Debug("sandbox: build", "output", strings.TrimSpace(msg.Stream))
		}
	}
}
