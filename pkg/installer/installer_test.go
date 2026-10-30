package installer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

type mockRegistry struct {
	registered   *UninstallRegistration
	unregistered string
	addedPath    string
	removedPath  string
}

func (m *mockRegistry) RegisterUninstall(reg UninstallRegistration) error {
	m.registered = &reg
	return nil
}

func (m *mockRegistry) UnregisterUninstall(appName string) error {
	m.unregistered = appName
	return nil
}

func (m *mockRegistry) AddToUserPath(dir string) error {
	m.addedPath = dir
	return nil
}

func (m *mockRegistry) RemoveFromUserPath(dir string) error {
	m.removedPath = dir
	return nil
}

func TestInstallAndUninstallLifecycle(t *testing.T) {
	tmpRoot := t.TempDir()
	installDir := filepath.Join(tmpRoot, "Programs", "TownCrier")
	shortcutRoot := filepath.Join(tmpRoot, "Shortcuts")
	userDataDir := filepath.Join(tmpRoot, "AppData", "com.github.mrpoundsign.towncrier")
	selfExe := filepath.Join(tmpRoot, "setup.exe")

	if err := os.MkdirAll(userDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDataDir, "config.json"), []byte(`{"version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selfExe, []byte("mock-setup-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	var hookRan bool
	spec := AppSpec{
		AppName:       "TownCrier",
		AppID:         "com.github.mrpoundsign.towncrier",
		Description:   "Local Neural TTS Stream Chat Reader",
		Publisher:     "mrpoundsign",
		Version:       "0.0.7",
		RepoOwner:     "mrpoundsign",
		RepoName:      "TownCrier",
		BinaryName:    "TownCrier.exe",
		PayloadSubdir: "payload",
		FileRenames: map[string]string{
			"towncrier-core.exe": "TownCrier.exe",
		},
		PayloadFS: fstest.MapFS{
			"payload/towncrier-core.exe": &fstest.MapFile{Data: []byte("core-exe-bytes")},
			"payload/onnxruntime.dll":    &fstest.MapFile{Data: []byte("onnx-dll-bytes")},
			"payload/LICENSE":            &fstest.MapFile{Data: []byte("AGPL-3.0")},
		},
		DataDirsToPurge: func() []string {
			return []string{userDataDir}
		},
		OptionalComponents: []ComponentSpec{
			{
				ID:        "supertonic",
				Title:     "Pre-download Supertonic TTS Model Pack",
				DefaultOn: true,
				Run: func(_ context.Context, _ string, progress func(cur, total int64, status string)) error {
					progress(50, 100, "Downloading Supertonic...")
					progress(100, 100, "Downloaded Supertonic")
					hookRan = true
					return nil
				},
			},
		},
	}

	reg := &mockRegistry{}
	ins := NewInstaller(spec)
	ins.Registry = reg
	ins.Shortcuts = NoopShortcutManager{BaseDir: shortcutRoot}

	manifest, err := ins.Install(context.Background(), Options{
		InstallDir:              installDir,
		CreateStartMenuShortcut: true,
		CreateDesktopShortcut:   true,
		AddToUserPath:           true,
		SelectedComponents:      map[string]bool{"supertonic": true},
		SelfExePath:             selfExe,
	}, nil)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	if !hookRan {
		t.Errorf("expected optional component hook 'supertonic' to run")
	}
	if reg.registered == nil || reg.registered.AppName != "TownCrier" || reg.registered.Version != "0.0.7" {
		t.Errorf("unexpected registry registration: %+v", reg.registered)
	}
	if reg.addedPath != installDir {
		t.Errorf("expected AddToUserPath(%q), got %q", installDir, reg.addedPath)
	}

	// Verify renamed TownCrier.exe and DLL exist
	for _, f := range []string{"TownCrier.exe", "onnxruntime.dll", "LICENSE", ManifestFileName} {
		if _, err := os.Stat(filepath.Join(installDir, f)); err != nil {
			t.Errorf("expected installed file %s to exist: %v", f, err)
		}
	}
	if len(manifest.Shortcuts) != 2 {
		t.Errorf("expected 2 shortcuts created, got %d", len(manifest.Shortcuts))
	}

	// Uninstall with purgeUserData = true
	if err := ins.Uninstall(context.Background(), installDir, true, nil); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	if reg.unregistered != "TownCrier" {
		t.Errorf("expected UnregisterUninstall('TownCrier'), got %q", reg.unregistered)
	}
	if reg.removedPath != installDir {
		t.Errorf("expected RemoveFromUserPath(%q), got %q", installDir, reg.removedPath)
	}
	if _, err := os.Stat(filepath.Join(installDir, "TownCrier.exe")); !os.IsNotExist(err) {
		t.Errorf("expected TownCrier.exe to be removed on uninstall")
	}
	if _, err := os.Stat(userDataDir); !os.IsNotExist(err) {
		t.Errorf("expected userDataDir to be purged when purgeUserData=true")
	}
}
