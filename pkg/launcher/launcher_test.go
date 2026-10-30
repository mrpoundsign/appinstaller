package launcher

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestExtractAndCleanupOldVersions(t *testing.T) {
	baseDir := t.TempDir()
	vOld1 := filepath.Join(baseDir, "0.0.5")
	vOld2 := filepath.Join(baseDir, "0.0.6")
	vCur := filepath.Join(baseDir, "0.0.7")

	for _, d := range []string{vOld1, vOld2, vCur} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "dummy.dll"), []byte("dll"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mockFS := fstest.MapFS{
		"payload/towncrier-core.exe": &fstest.MapFile{Data: []byte("core-binary-v007")},
		"payload/onnxruntime.dll":    &fstest.MapFile{Data: []byte("onnx-dll-v007")},
		"payload/placeholder.txt":    &fstest.MapFile{Data: []byte("ignore")},
	}

	if err := ExtractPayloadIfNeeded(mockFS, "payload", vCur); err != nil {
		t.Fatalf("ExtractPayloadIfNeeded failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(vCur, "towncrier-core.exe")); err != nil {
		t.Errorf("expected towncrier-core.exe to be extracted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vCur, "placeholder.txt")); !os.IsNotExist(err) {
		t.Errorf("expected placeholder.txt to be skipped")
	}

	// Second extraction should hit SHA-256 stream cache cleanly
	if err := ExtractPayloadIfNeeded(mockFS, "payload", vCur); err != nil {
		t.Fatalf("second ExtractPayloadIfNeeded failed: %v", err)
	}

	if err := CleanupOldVersions(baseDir, "0.0.7"); err != nil {
		t.Fatalf("CleanupOldVersions failed: %v", err)
	}

	if _, err := os.Stat(vOld1); !os.IsNotExist(err) {
		t.Errorf("expected old version dir 0.0.5 to be removed")
	}
	if _, err := os.Stat(vOld2); !os.IsNotExist(err) {
		t.Errorf("expected old version dir 0.0.6 to be removed")
	}
	if _, err := os.Stat(vCur); err != nil {
		t.Errorf("expected current version dir 0.0.7 to remain: %v", err)
	}
}

func TestCleanupOldPortableBinary(t *testing.T) {
	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "TownCrier.exe")
	oldPath := exePath + ".old"

	if err := os.WriteFile(oldPath, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	CleanupOldPortableBinary(exePath)

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expected .old file to be removed")
	}
}
