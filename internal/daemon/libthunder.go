package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	// libthunderCacheSubdir holds one copy of the library per release, shared
	// by every claim on the node.
	libthunderCacheSubdir = "lib"

	// installerReadLimit caps the installer fetch. Installers are ~13KB.
	installerReadLimit = int64(1 << 20)
	// libthunderSizeLimit caps the library download, well above its ~32MB.
	libthunderSizeLimit = int64(1 << 29)
)

// The Thunder installer pins the libthunder.so it would install by SHA-256 and
// serves it from an artifact host it also declares. Reading the pin out of the
// installer keeps a node on exactly the build `curl | sh` would have produced,
// without hardcoding a digest that goes stale on every Thunder release. Both
// values can be overridden when a deployment serves its own artifacts.
var (
	libthunderSHAPattern = regexp.MustCompile(`libthunder_sha256="([0-9a-f]{64})"`)
	artifactBasePattern  = regexp.MustCompile(`default_artifact_base_url='([^']+)'`)
)

// LibthunderArtifact identifies one build of the client library.
type LibthunderArtifact struct {
	URL    string
	SHA256 string
}

// LibthunderCache resolves and caches libthunder.so on a node.
type LibthunderCache struct {
	// Dir is the cache root. It must be readable by the CDI hook, which means
	// somewhere under the kubelet plugin directory.
	Dir string
	// InstallURL is the installer whose pin is authoritative. Ignored when
	// both URL and SHA256 are set.
	InstallURL string
	// URL and SHA256 pin the library explicitly, skipping the installer.
	URL    string
	SHA256 string
	// ArtifactBaseURL overrides only the host, keeping the installer's digest.
	ArtifactBaseURL string
	// HTTP is used for both fetches. Nil means http.DefaultClient.
	HTTP *http.Client

	mu       sync.Mutex
	resolved *LibthunderArtifact
}

func (c *LibthunderCache) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *LibthunderCache) installURL() string {
	if strings.TrimSpace(c.InstallURL) != "" {
		return strings.TrimSpace(c.InstallURL)
	}
	return DefaultThunderInstallURL
}

// Resolve reports which build of libthunder.so this node should stage. The
// answer is memoised, since it only changes when Thunder ships a release.
func (c *LibthunderCache) Resolve(ctx context.Context) (LibthunderArtifact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved != nil {
		return *c.resolved, nil
	}

	// An explicit pin skips the installer entirely, so an air-gapped or
	// self-hosted deployment never has to reach get.thundercompute.com.
	if strings.TrimSpace(c.URL) != "" && strings.TrimSpace(c.SHA256) != "" {
		artifact := LibthunderArtifact{URL: strings.TrimSpace(c.URL), SHA256: strings.TrimSpace(c.SHA256)}
		c.resolved = &artifact
		return artifact, nil
	}

	installer, err := c.fetchInstaller(ctx)
	if err != nil {
		return LibthunderArtifact{}, err
	}
	artifact, err := parseLibthunderArtifact(installer, c.ArtifactBaseURL)
	if err != nil {
		return LibthunderArtifact{}, fmt.Errorf("%w (installer %s)", err, c.installURL())
	}
	c.resolved = &artifact
	return artifact, nil
}

func (c *LibthunderCache) fetchInstaller(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.installURL(), nil)
	if err != nil {
		return "", fmt.Errorf("build installer request: %w", err)
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch installer %s: %w", c.installURL(), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch installer %s: unexpected status %s", c.installURL(), response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, installerReadLimit))
	if err != nil {
		return "", fmt.Errorf("read installer %s: %w", c.installURL(), err)
	}
	return string(body), nil
}

// parseLibthunderArtifact pulls the digest and download URL out of an installer.
func parseLibthunderArtifact(installer, baseOverride string) (LibthunderArtifact, error) {
	shaMatch := libthunderSHAPattern.FindStringSubmatch(installer)
	if len(shaMatch) != 2 {
		return LibthunderArtifact{}, fmt.Errorf("installer does not pin libthunder_sha256")
	}

	base := strings.TrimSpace(baseOverride)
	if base == "" {
		baseMatch := artifactBasePattern.FindStringSubmatch(installer)
		if len(baseMatch) != 2 {
			return LibthunderArtifact{}, fmt.Errorf("installer does not declare default_artifact_base_url")
		}
		base = baseMatch[1]
	}

	return LibthunderArtifact{
		URL:    strings.TrimSuffix(base, "/") + "/libthunder.so",
		SHA256: shaMatch[1],
	}, nil
}

// Path is where a build is cached, whether or not it has been downloaded. The
// digest is in the name, so a new release lands as a new file rather than
// silently replacing the one running containers were staged from.
func (c *LibthunderCache) Path(artifact LibthunderArtifact) string {
	return filepath.Join(c.Dir, libthunderCacheSubdir, "libthunder-"+artifact.SHA256+".so")
}

// Ensure downloads libthunder.so unless the node already has that exact build,
// and returns the path to it. The digest is verified before the file is
// published, so a truncated or tampered download never becomes the library a
// workload preloads.
func (c *LibthunderCache) Ensure(ctx context.Context) (string, error) {
	artifact, err := c.Resolve(ctx)
	if err != nil {
		return "", err
	}
	path := c.Path(artifact)
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create libthunder cache dir: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build libthunder request: %w", err)
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", artifact.URL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: unexpected status %s", artifact.URL, response.Status)
	}

	// Download beside the target and rename, so hooks running concurrently for
	// other claims never observe a half-written library at the cache path.
	temp, err := os.CreateTemp(filepath.Dir(path), ".libthunder-*")
	if err != nil {
		return "", fmt.Errorf("create libthunder temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(response.Body, libthunderSizeLimit)); err != nil {
		temp.Close()
		return "", fmt.Errorf("write libthunder: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close libthunder temp file: %w", err)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != artifact.SHA256 {
		return "", fmt.Errorf("libthunder digest mismatch: downloaded %s, expected %s", got, artifact.SHA256)
	}
	if err := os.Chmod(tempName, 0o755); err != nil {
		return "", fmt.Errorf("chmod libthunder: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return "", fmt.Errorf("publish libthunder: %w", err)
	}
	return path, nil
}
