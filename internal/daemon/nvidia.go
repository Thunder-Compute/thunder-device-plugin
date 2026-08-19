package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func nvidiaChecks(ctx context.Context, cfg Config, runner commandRunner) (int, string, error) {
	for _, nodePath := range []string{cfg.LibCUDAPath, cfg.LibNVMLPath, cfg.NVSMIPath} {
		resolved := resolveNodePath(cfg.HostRoot, nodePath)
		if !fileExists(resolved) {
			return 0, "", fileNotFoundError(nodePath, resolved)
		}
	}

	driverVersion, err := nvidiaDriverVersion(ctx, cfg, runner)
	if err != nil {
		return 0, "", err
	}
	if err := ensureVersionRecency(driverVersion, cfg.MinDriverVersion); err != nil {
		return 0, "", err
	}

	gpuCount, err := nvidiaGPUCount(ctx, cfg, runner)
	if err != nil {
		return 0, "", err
	}
	if gpuCount == 0 {
		return 0, "", errors.New("nvidia-smi reported zero GPU devices")
	}
	return gpuCount, driverVersion, nil
}

func nvidiaDriverVersion(ctx context.Context, cfg Config, runner commandRunner) (string, error) {
	output, err := runner.CombinedOutput(ctx, cfg.NVSMIPath, "--query-gpu=driver_version", "--format=csv,noheader,nounits")
	if err != nil {
		return "", fmt.Errorf("get nvidia driver version: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		version := strings.TrimSpace(line)
		if version != "" {
			return version, nil
		}
	}
	return "", errors.New("nvidia-smi did not report a driver version")
}

func nvidiaGPUCount(ctx context.Context, cfg Config, runner commandRunner) (int, error) {
	output, err := runner.CombinedOutput(ctx, cfg.NVSMIPath, "--query-gpu=index", "--format=csv,noheader,nounits")
	if err != nil {
		return 0, fmt.Errorf("get nvidia gpu count: %w: %s", err, strings.TrimSpace(string(output)))
	}

	count := 0
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}

func ensureVersionRecency(version string, minVersion string) error {
	versionParts, err := parseVersion(version)
	if err != nil {
		return fmt.Errorf("nvidia driver format is incorrect: %s; %w", version, err)
	}
	minVersionParts, err := parseVersion(minVersion)
	if err != nil {
		return fmt.Errorf("%s env variable format is incorrect: %s; %w", EnvMinNVDriverVersion, minVersion, err)
	}

	maxLen := len(versionParts)
	if len(minVersionParts) > maxLen {
		maxLen = len(minVersionParts)
	}
	for i := 0; i < maxLen; i++ {
		versionNum := versionPart(versionParts, i)
		minVersionNum := versionPart(minVersionParts, i)
		if versionNum > minVersionNum {
			return nil
		}
		if versionNum < minVersionNum {
			return fmt.Errorf("version not accepted. machine has version %s, thunder expects at least %s", version, minVersion)
		}
	}
	return nil
}

func parseVersion(version string) ([]int, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, errors.New("empty version")
	}

	parts := strings.Split(version, ".")
	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return nil, fmt.Errorf("empty version component in %q", version)
		}
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

func versionPart(parts []int, index int) int {
	if index >= len(parts) {
		return 0
	}
	return parts[index]
}

func fileNotFoundError(nodePath string, resolvedPath string) error {
	return fmt.Errorf("%s not found at node path %s (resolved path %s)", filepath.Base(nodePath), nodePath, resolvedPath)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return false
}

func resolveNodePath(hostRoot string, nodePath string) string {
	cleanNodePath := filepath.Clean(nodePath)
	if hostRoot == "" || hostRoot == "/" || !filepath.IsAbs(cleanNodePath) {
		return cleanNodePath
	}
	return filepath.Join(hostRoot, strings.TrimPrefix(cleanNodePath, string(os.PathSeparator)))
}

const maxHostViewSymlinks = 40

func pathInHostRoot(hostRoot string, path string) bool {
	if path == hostRoot {
		return true
	}
	return strings.HasPrefix(path, hostRoot+string(os.PathSeparator))
}

// statNodePath stats a node path through hostRoot. Absolute symlink targets
// are re-rooted under hostRoot so they are followed in the host tree, not the
// daemon's filesystem. os.Stat on /host/usr/local/cuda would otherwise resolve
// a typical NVIDIA /usr/local/cuda -> /usr/local/cuda-X.Y link into the
// container root and miss the host toolkit.
func statNodePath(hostRoot string, nodePath string) (os.FileInfo, error) {
	resolved, err := resolveHostViewPath(hostRoot, nodePath)
	if err != nil {
		return nil, err
	}
	return os.Lstat(resolved)
}

func resolveHostViewPath(hostRoot string, nodePath string) (string, error) {
	cleanNodePath := filepath.Clean(nodePath)
	if hostRoot == "" || hostRoot == "/" || !filepath.IsAbs(cleanNodePath) {
		return cleanNodePath, nil
	}
	hostRoot = filepath.Clean(hostRoot)

	components := strings.Split(strings.TrimPrefix(cleanNodePath, string(os.PathSeparator)), string(os.PathSeparator))
	current := hostRoot
	linksFollowed := 0
	for i := 0; i < len(components); i++ {
		component := components[i]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			if current != hostRoot {
				parent := filepath.Dir(current)
				if pathInHostRoot(hostRoot, parent) {
					current = parent
				} else {
					current = hostRoot
				}
			}
			continue
		}

		next := filepath.Join(current, component)
		info, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}

		linksFollowed++
		if linksFollowed > maxHostViewSymlinks {
			return "", fmt.Errorf("too many levels of symbolic links at %s", next)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		target = filepath.Clean(target)
		var rest []string
		if filepath.IsAbs(target) {
			current = hostRoot
			rest = strings.Split(strings.TrimPrefix(target, string(os.PathSeparator)), string(os.PathSeparator))
		} else {
			current = filepath.Dir(next)
			if !pathInHostRoot(hostRoot, current) {
				current = hostRoot
			}
			rest = strings.Split(target, string(os.PathSeparator))
		}
		components = append(rest, components[i+1:]...)
		i = -1
	}
	return current, nil
}
