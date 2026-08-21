package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const serverEnrollmentStateFilename = "server-enrollment.json"

// serverEnrollmentState remembers the Central object created for this node.
// The enrollment secret is deliberately not retained: only the ID needed to
// revoke the enrollment and its resulting server belongs on disk.
type serverEnrollmentState struct {
	EnrollmentTokenID string `json:"enrollmentTokenId"`
	Node              string `json:"node"`
	ZoneID            string `json:"zoneId"`
}

func serverEnrollmentStatePath(cfg Config) string {
	return filepath.Join(cfg.KubeletPluginDir, serverEnrollmentStateFilename)
}

func readServerEnrollmentState(path string) (serverEnrollmentState, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return serverEnrollmentState{}, false, nil
	}
	if err != nil {
		return serverEnrollmentState{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	var state serverEnrollmentState
	if err := json.Unmarshal(raw, &state); err != nil {
		return serverEnrollmentState{}, false, fmt.Errorf("decode %s: %w", path, err)
	}
	state.EnrollmentTokenID = strings.TrimSpace(state.EnrollmentTokenID)
	if state.EnrollmentTokenID == "" {
		return serverEnrollmentState{}, false, fmt.Errorf("decode %s: enrollmentTokenId is empty", path)
	}
	return state, true, nil
}

// writeServerEnrollmentState atomically replaces the state file. A crash can
// therefore leave either the old complete record or the new complete record,
// never a partial ID that cannot be revoked on the next pass.
func writeServerEnrollmentState(path string, state serverEnrollmentState) error {
	if strings.TrimSpace(state.EnrollmentTokenID) == "" {
		return fmt.Errorf("write %s: enrollmentTokenId is empty", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create server enrollment state directory %s: %w", dir, err)
	}

	temporary, err := os.CreateTemp(dir, ".server-enrollment-*")
	if err != nil {
		return fmt.Errorf("create temporary server enrollment state in %s: %w", dir, err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("protect temporary server enrollment state: %w", err)
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
		return fmt.Errorf("encode temporary server enrollment state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary server enrollment state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary server enrollment state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace server enrollment state %s: %w", path, err)
	}
	// Rename is the persistence boundary the rest of enroll relies on: the
	// replacement ID is already at the final path. Directory sync is
	// durability only, and returning it as a failed write would revoke a
	// tracked enrollment while leaving the state file in place.
	_ = syncDirectory(dir)
	return nil
}

func removeServerEnrollmentState(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove server enrollment state %s: %w", path, err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open state directory %s for sync: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync state directory %s: %w", path, err)
	}
	return nil
}
