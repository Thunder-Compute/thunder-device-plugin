package daemon

import (
	"fmt"
	"os"
	"strings"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/hostartifacts"
)

func effectiveHostArtifactProfile(profile hostartifacts.Profile) hostartifacts.Profile {
	if profile == "" {
		return hostartifacts.ProfileDriver
	}
	return profile
}

func (s *FileCDIDeviceStore) toolkitPath() string {
	if strings.TrimSpace(s.ToolkitPath) == "" {
		return DefaultHostArtifactToolkit
	}
	return strings.TrimSpace(s.ToolkitPath)
}

// validateMounts checks the daemon's view through /host before creating token
// state. HostRoot is always set in production; leaving it empty keeps the CDI
// store usable as a pure spec writer in unit tests and other library callers.
func (s *FileCDIDeviceStore) validateMounts(profile hostartifacts.Profile) error {
	profile = effectiveHostArtifactProfile(profile)
	if _, err := hostartifacts.ParseProfile(string(profile)); err != nil {
		return err
	}
	if strings.TrimSpace(s.HostRoot) == "" || profile == hostartifacts.ProfileNone {
		return nil
	}

	paths := []string{s.nvidiaSMIPath(), s.libCUDAPath(), s.libNVMLPath()}
	if profile == hostartifacts.ProfileFull {
		paths = append(paths, s.toolkitPath())
	}
	for _, nodePath := range paths {
		resolved := resolveNodePath(s.HostRoot, nodePath)
		info, err := os.Stat(resolved)
		if err != nil {
			if os.IsNotExist(err) {
				return fileNotFoundError(nodePath, resolved)
			}
			return fmt.Errorf("inspect node path %s (resolved path %s): %w", nodePath, resolved, err)
		}
		if nodePath == s.toolkitPath() && profile == hostartifacts.ProfileFull && !info.IsDir() {
			return fmt.Errorf("host artifact toolkit path %s (resolved path %s) is not a directory", nodePath, resolved)
		}
	}
	return nil
}
