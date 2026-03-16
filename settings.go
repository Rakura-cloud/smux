package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type AppSettings struct {
	PanePresetIdx int  `json:"pane_preset_idx"`
	SidebarHidden bool `json:"sidebar_hidden"`
}

var settingsMu sync.Mutex

func settingsPath() string {
	return filepath.Join(socketDir(), "settings.json")
}

func loadSettings() AppSettings {
	settings := AppSettings{PanePresetIdx: 0, SidebarHidden: false}
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return settings
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return AppSettings{PanePresetIdx: 0, SidebarHidden: false}
	}
	if settings.PanePresetIdx < 0 || settings.PanePresetIdx >= len(paneWidthPresets) {
		settings.PanePresetIdx = 0
	}
	return settings
}

func saveSettings(settings AppSettings) {
	if settings.PanePresetIdx < 0 || settings.PanePresetIdx >= len(paneWidthPresets) {
		settings.PanePresetIdx = 0
	}
	if err := os.MkdirAll(socketDir(), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return
	}
	tmp := settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, settingsPath())
}

func updateSettings(fn func(*AppSettings)) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	settings := loadSettings()
	fn(&settings)
	saveSettings(settings)
}
