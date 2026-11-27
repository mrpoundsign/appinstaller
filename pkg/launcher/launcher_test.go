package launcher

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/klauspost/compress/zstd"
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

func TestExtractCompressedPayloadAndStampFastPath(t *testing.T) {
	makeTar := func(files map[string]string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			if err := tw.WriteHeader(&tar.Header{
				Name:     name,
				Typeflag: tar.TypeReg,
				Size:     int64(len(body)),
				Mode:     0o755,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	makeTarZst := func(files map[string]string) []byte {
		var out bytes.Buffer
		zw, err := zstd.NewWriter(&out)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := zw.Write(makeTar(files)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}

	makeTarGz := func(files map[string]string) []byte {
		var out bytes.Buffer
		gw := gzip.NewWriter(&out)
		if _, err := gw.Write(makeTar(files)); err != nil {
			t.Fatal(err)
		}
		if err := gw.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}

	makeZip := func(files map[string]string) []byte {
		var out bytes.Buffer
		zw := zip.NewWriter(&out)
		for name, body := range files {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}

	t.Run("tar_zst_stamp_lifecycle", func(t *testing.T) {
		targetDir := t.TempDir()
		v1 := makeTarZst(map[string]string{
			"towncrier-core.exe": "core-v1",
			"onnxruntime.dll":    "dll-v1",
		})
		fsys := fstest.MapFS{
			"payload/payload.tar.zst": &fstest.MapFile{Data: v1},
		}

		if err := ExtractPayloadIfNeeded(fsys, "payload", targetDir); err != nil {
			t.Fatalf("first ExtractPayloadIfNeeded failed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(targetDir, payloadStampFileName)); err != nil {
			t.Fatalf("expected stamp file %s: %v", payloadStampFileName, err)
		}

		exePath := filepath.Join(targetDir, "towncrier-core.exe")
		fixedTime := time.Unix(1700000000, 0)
		if err := os.Chtimes(exePath, fixedTime, fixedTime); err != nil {
			t.Fatal(err)
		}

		// Second call with identical archive and existing files must skip extraction.
		if err := ExtractPayloadIfNeeded(fsys, "payload", targetDir); err != nil {
			t.Fatalf("second ExtractPayloadIfNeeded failed: %v", err)
		}
		info, err := os.Stat(exePath)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(fixedTime) {
			t.Fatalf("expected stamp fast-path to skip re-extraction; modtime changed from %v to %v", fixedTime, info.ModTime())
		}

		// Deleting one extracted file should trigger re-extraction.
		if err := os.Remove(filepath.Join(targetDir, "onnxruntime.dll")); err != nil {
			t.Fatal(err)
		}
		if err := ExtractPayloadIfNeeded(fsys, "payload", targetDir); err != nil {
			t.Fatalf("re-extract after missing file failed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(targetDir, "onnxruntime.dll")); err != nil {
			t.Fatalf("expected onnxruntime.dll to be restored: %v", err)
		}

		// Changing archive bytes should invalidate stamp and overwrite extracted files.
		v2 := makeTarZst(map[string]string{
			"towncrier-core.exe": "core-v2",
			"onnxruntime.dll":    "dll-v2",
		})
		fsys["payload/payload.tar.zst"] = &fstest.MapFile{Data: v2}
		if err := ExtractPayloadIfNeeded(fsys, "payload", targetDir); err != nil {
			t.Fatalf("re-extract after archive update failed: %v", err)
		}
		got, err := os.ReadFile(exePath)
		if err != nil || string(got) != "core-v2" {
			t.Fatalf("expected core-v2 after archive update, got %q, err=%v", got, err)
		}
	})

	t.Run("tar_gz_and_zip_formats", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			path string
			data []byte
		}{
			{name: "tar.gz", path: "payload/payload.tar.gz", data: makeTarGz(map[string]string{"app.exe": "gz"})},
			{name: "zip", path: "payload/payload.zip", data: makeZip(map[string]string{"app.exe": "zip"})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				fsys := fstest.MapFS{tc.path: &fstest.MapFile{Data: tc.data}}
				if err := ExtractPayloadIfNeeded(fsys, "payload", dir); err != nil {
					t.Fatalf("ExtractPayloadIfNeeded(%s) failed: %v", tc.name, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "app.exe")); err != nil {
					t.Fatalf("expected app.exe: %v", err)
				}
			})
		}
	})
}
