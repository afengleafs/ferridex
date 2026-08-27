package server

import (
	"errors"
	"os"
	"path/filepath"
)

// ensurePrivateProviderFile creates or fills an empty provider file without
// ever overwriting non-empty user configuration. migration wins over the
// commented template when legacy configuration is available.
func ensurePrivateProviderFile(path string, migration []byte, template string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.Size() > 0:
		return false, os.Chmod(path, 0o600)
	case err == nil:
		// A zero-byte file is treated as an explicit placeholder and may be
		// initialized from legacy data or the safe commented template.
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return false, err
	}

	data := migration
	migrated := len(data) > 0
	if !migrated {
		data = []byte(template)
	}
	if err := writePrivateProviderFile(path, data); err != nil {
		return false, err
	}
	return migrated, nil
}

func writePrivateProviderFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".provider-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
