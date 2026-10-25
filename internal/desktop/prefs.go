package desktop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"icaro/internal/paths"
)

// StoredPrefs are the app's own settings. Launch at login is not stored
// here: the platform's registration (a login item) is the source of truth.
type StoredPrefs struct {
	// TrayOnly opens no window at launch, including at login; the tray
	// is the whole app until "Open Ícaro" is chosen.
	TrayOnly bool `json:"tray_only"`
}

// PrefsPath is where the app keeps its settings.
func PrefsPath(l paths.Layout) string { return filepath.Join(l.ConfigDir(), "desktop.json") }

// LoadPrefs reads the settings; a missing file is the defaults.
func LoadPrefs(path string) (StoredPrefs, error) {
	var p StoredPrefs
	b, err := os.ReadFile(path) //nolint:gosec // our own settings file
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

// SavePrefs writes the settings atomically.
func SavePrefs(path string, p StoredPrefs) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
