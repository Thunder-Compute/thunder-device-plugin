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
