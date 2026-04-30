package k3s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	dockerpkg "github.com/bRRRITSCOLD/burrow/internal/sandbox/docker"
	dockerimage "github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
)

// builder wraps the local Docker ImageBuilder and pushes built images to the
// configured registry so containerd in k3s can pull them.
type builder struct {
	docker   *dockerpkg.ImageBuilder
	dcli     dockerclient.APIClient
	registry string
}

// newBuilder attempts to connect to the local Docker daemon. Returns nil if
// Docker is unreachable (packages will then return an error at use time).
func newBuilder(registry string) *builder {
	if registry == "" {
		return nil
	}
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		slog.Warn("sandbox/k3s: docker client unavailable; package builds disabled", "err", err)
		return nil
	}
	return &builder{
		docker:   dockerpkg.NewImageBuilder(cli, registry),
		dcli:     cli,
		registry: registry,
	}
}

// resolvePackageImage builds (if needed) and pushes a package-augmented image,
// returning the registry-qualified tag to reference from a Pod spec.
//
// Steps:
//  1. Compute deterministic tag.
//  2. HEAD registry manifest — skip build/push if already present.
//  3. Build via local Docker daemon (uses existing Dockerfile generator).
//  4. Push to local registry over plain HTTP.
func (b *builder) resolvePackageImage(ctx context.Context, lang sandbox.Language, base string, packages []string, debug bool) (string, error) {
	if b == nil {
		return "", fmt.Errorf("sandbox/k3s: package builder unavailable (no Docker daemon access)")
	}

	// 1. Build locally — produces a registry-prefixed tag.
	tag, err := b.docker.BuildOrReuse(ctx, lang, base, packages, debug)
	if err != nil {
		return "", err
	}

	// 2. Skip push if registry already has this manifest.
	if has, _ := b.registryHas(ctx, tag); has {
		return tag, nil
	}

	// 3. Push to registry. Docker Push expects an empty X-Registry-Auth for
	// anonymous registries; pass "{}" base64-encoded which Docker decodes as
	// no-auth credentials.
	pushResp, err := b.dcli.ImagePush(ctx, tag, dockerimage.PushOptions{
		RegistryAuth: "e30K", // base64("{}")
	})
	if err != nil {
		return "", fmt.Errorf("sandbox/k3s: push %s: %w", tag, err)
	}
	defer func() { _ = pushResp.Close() }()
	// Drain push output (Docker streams progress JSON; we only care about errors,
	// which surface as final ImagePush error or a JSON {"error":"..."} message).
	if err := drainPush(pushResp); err != nil {
		return "", fmt.Errorf("sandbox/k3s: push %s: %w", tag, err)
	}

	slog.Info("sandbox/k3s: pushed package image", "tag", tag)
	return tag, nil
}

// registryHas checks whether the registry already has this manifest. Best-effort.
func (b *builder) registryHas(ctx context.Context, tag string) (bool, error) {
	repo, ref, ok := splitTag(tag)
	if !ok {
		return false, fmt.Errorf("malformed tag: %s", tag)
	}
	url := fmt.Sprintf("http://%s/v2/%s/manifests/%s", b.registry, repo, ref)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}

// splitTag extracts the repository and reference from a registry-qualified tag,
// e.g. "localhost:5000/burrow/sandbox-pkg:abcd" → "burrow/sandbox-pkg", "abcd".
func splitTag(tag string) (repo, ref string, ok bool) {
	slash := strings.Index(tag, "/")
	if slash < 0 {
		return "", "", false
	}
	rest := tag[slash+1:]
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		return rest, "latest", true
	}
	return rest[:colon], rest[colon+1:], true
}

// drainPush reads the Docker push progress stream and returns the first error
// JSON message it sees, or nil on clean completion.
func drainPush(rc io.Reader) error {
	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			Error string `json:"error"`
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
	}
}
