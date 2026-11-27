package payloadarchive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/klauspost/compress/zstd"
)

type testArchiveEntry struct {
	name     string
	data     []byte
	typeflag byte
	linkname string
	mode     os.FileMode
}

func buildTestTar(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Size:     int64(len(e.data)),
			Mode:     0o755,
			Linkname: e.linkname,
		}
		if tf == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.name, err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatalf("Write(%s): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

func buildTestTarZst(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	rawTar := buildTestTar(t, entries)
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := zw.Write(rawTar); err != nil {
		t.Fatalf("zstd Write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd Close: %v", err)
	}
	return out.Bytes()
}

func buildTestTarGz(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	rawTar := buildTestTar(t, entries)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(rawTar); err != nil {
		t.Fatalf("gzip Write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return out.Bytes()
}

func buildTestZip(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, e := range entries {
		hdr := &zip.FileHeader{
			Name:   e.name,
			Method: zip.Deflate,
		}
		if e.mode != 0 {
			hdr.SetMode(e.mode)
		} else {
			hdr.SetMode(0o755)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("zip CreateHeader(%s): %v", e.name, err)
		}
		if len(e.data) > 0 {
			if _, err := w.Write(e.data); err != nil {
				t.Fatalf("zip Write(%s): %v", e.name, err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return out.Bytes()
}

func TestDetectArchivePriorityAndHash(t *testing.T) {
	fsys := fstest.MapFS{
		"payload/payload.zip":     &fstest.MapFile{Data: []byte("zip-bytes")},
		"payload/payload.tar.gz":  &fstest.MapFile{Data: []byte("gz-bytes")},
		"payload/payload.tar.zst": &fstest.MapFile{Data: []byte("zst-bytes")},
	}

	p, format, ok := DetectArchive(fsys, "payload")
	if !ok || format != FormatTarZst || p != "payload/payload.tar.zst" {
		t.Fatalf("expected payload.tar.zst first, got ok=%v format=%q path=%q", ok, format, p)
	}

	digest, size, err := HashFile(fsys, p)
	if err != nil {
		t.Fatalf("HashFile failed: %v", err)
	}
	wantSum := sha256.Sum256([]byte("zst-bytes"))
	if digest != hex.EncodeToString(wantSum[:]) || size != int64(len("zst-bytes")) {
		t.Fatalf("unexpected digest/size: got (%s, %d)", digest, size)
	}

	delete(fsys, "payload/payload.tar.zst")
	p, format, ok = DetectArchive(fsys, "payload")
	if !ok || format != FormatTarGz || p != "payload/payload.tar.gz" {
		t.Fatalf("expected payload.tar.gz second, got ok=%v format=%q path=%q", ok, format, p)
	}

	delete(fsys, "payload/payload.tar.gz")
	p, format, ok = DetectArchive(fsys, "payload")
	if !ok || format != FormatZip || p != "payload/payload.zip" {
		t.Fatalf("expected payload.zip third, got ok=%v format=%q path=%q", ok, format, p)
	}

	delete(fsys, "payload/payload.zip")
	if _, _, ok := DetectArchive(fsys, "payload"); ok {
		t.Fatal("expected no archive detected when only empty dir remains")
	}
}

func TestExtractAllFormatsWithRenamesAndNestedDirs(t *testing.T) {
	entries := []testArchiveEntry{
		{name: "placeholder.txt", data: []byte("ignore")},
		{name: "app-core.exe", data: []byte("core-exe-bytes")},
		{name: "docs/", typeflag: tar.TypeDir, mode: os.ModeDir | 0o755},
		{name: "docs/guide.md", data: []byte("# Guide")},
		{name: "accessories/hat.svg", data: []byte("<svg/>")},
	}

	formats := []struct {
		name   string
		format Format
		file   string
		data   []byte
	}{
		{name: "tar.zst", format: FormatTarZst, file: "payload/payload.tar.zst", data: buildTestTarZst(t, entries)},
		{name: "tar.gz", format: FormatTarGz, file: "payload/payload.tar.gz", data: buildTestTarGz(t, entries)},
		{name: "zip", format: FormatZip, file: "payload/payload.zip", data: buildTestZip(t, entries)},
	}

	for _, tc := range formats {
		t.Run(tc.name, func(t *testing.T) {
			targetDir := t.TempDir()
			fsys := fstest.MapFS{
				tc.file: &fstest.MapFile{Data: tc.data},
			}

			var lastRead int64 = -1
			var progressCalls int
			res, err := Extract(context.Background(), fsys, tc.file, tc.format, ExtractOptions{
				TargetDir: targetDir,
				FileRenames: map[string]string{
					"app-core.exe": "MyApp.exe",
				},
				OnFileStart: func(_ string, bytesRead, totalCompressed int64) {
					progressCalls++
					if bytesRead < lastRead {
						t.Fatalf("bytesRead went backwards: %d < %d", bytesRead, lastRead)
					}
					if totalCompressed <= 0 {
						t.Fatalf("expected totalCompressed > 0, got %d", totalCompressed)
					}
					lastRead = bytesRead
				},
			})
			if err != nil {
				t.Fatalf("Extract(%s) failed: %v", tc.name, err)
			}

			if progressCalls != 3 {
				t.Errorf("expected 3 OnFileStart calls, got %d", progressCalls)
			}

			wantFiles := []string{"MyApp.exe", "docs/guide.md", "accessories/hat.svg"}
			if !slices.Equal(res.Files, wantFiles) {
				t.Errorf("got files %v, want %v", res.Files, wantFiles)
			}

			if _, err := os.Stat(filepath.Join(targetDir, "placeholder.txt")); !os.IsNotExist(err) {
				t.Errorf("expected placeholder.txt to be skipped")
			}

			gotExe, err := os.ReadFile(filepath.Join(targetDir, "MyApp.exe"))
			if err != nil || string(gotExe) != "core-exe-bytes" {
				t.Errorf("unexpected MyApp.exe content: %q, err=%v", gotExe, err)
			}
			gotGuide, err := os.ReadFile(filepath.Join(targetDir, "docs", "guide.md"))
			if err != nil || string(gotGuide) != "# Guide" {
				t.Errorf("unexpected docs/guide.md content: %q, err=%v", gotGuide, err)
			}
			gotHat, err := os.ReadFile(filepath.Join(targetDir, "accessories", "hat.svg"))
			if err != nil || string(gotHat) != "<svg/>" {
				t.Errorf("unexpected accessories/hat.svg content: %q, err=%v", gotHat, err)
			}
		})
	}
}

func TestExtractRejectsZipSlipSymlinksAndCorruptArchives(t *testing.T) {
	badPaths := []string{
		"../escape.txt",
		"a/../../escape.txt",
		"/abs/path.txt",
		`C:\evil.exe`,
	}

	for _, bad := range badPaths {
		t.Run("tar_path_"+bad, func(t *testing.T) {
			data := buildTestTarZst(t, []testArchiveEntry{
				{name: bad, data: []byte("evil")},
			})
			fsys := fstest.MapFS{
				"payload/payload.tar.zst": &fstest.MapFile{Data: data},
			}
			_, err := Extract(context.Background(), fsys, "payload/payload.tar.zst", FormatTarZst, ExtractOptions{
				TargetDir: t.TempDir(),
			})
			if err == nil {
				t.Fatalf("expected error for unsafe tar path %q", bad)
			}
		})

		t.Run("zip_path_"+bad, func(t *testing.T) {
			data := buildTestZip(t, []testArchiveEntry{
				{name: bad, data: []byte("evil")},
			})
			fsys := fstest.MapFS{
				"payload/payload.zip": &fstest.MapFile{Data: data},
			}
			_, err := Extract(context.Background(), fsys, "payload/payload.zip", FormatZip, ExtractOptions{
				TargetDir: t.TempDir(),
			})
			if err == nil {
				t.Fatalf("expected error for unsafe zip path %q", bad)
			}
		})
	}

	t.Run("tar_symlink_rejected", func(t *testing.T) {
		data := buildTestTarZst(t, []testArchiveEntry{
			{name: "link.txt", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		})
		fsys := fstest.MapFS{
			"payload/payload.tar.zst": &fstest.MapFile{Data: data},
		}
		_, err := Extract(context.Background(), fsys, "payload/payload.tar.zst", FormatTarZst, ExtractOptions{
			TargetDir: t.TempDir(),
		})
		if err == nil || !strings.Contains(err.Error(), "link") {
			t.Fatalf("expected link rejection error, got %v", err)
		}
	})

	t.Run("zip_symlink_rejected", func(t *testing.T) {
		data := buildTestZip(t, []testArchiveEntry{
			{name: "link.txt", data: []byte("/etc/passwd"), mode: os.ModeSymlink | 0o777},
		})
		fsys := fstest.MapFS{
			"payload/payload.zip": &fstest.MapFile{Data: data},
		}
		_, err := Extract(context.Background(), fsys, "payload/payload.zip", FormatZip, ExtractOptions{
			TargetDir: t.TempDir(),
		})
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("expected symlink rejection error, got %v", err)
		}
	})

	t.Run("corrupt_zst", func(t *testing.T) {
		fsys := fstest.MapFS{
			"payload/payload.tar.zst": &fstest.MapFile{Data: []byte("not-a-valid-zstd-stream")},
		}
		_, err := Extract(context.Background(), fsys, "payload/payload.tar.zst", FormatTarZst, ExtractOptions{
			TargetDir: t.TempDir(),
		})
		if err == nil {
			t.Fatal("expected error for corrupt zstd archive")
		}
	})

	t.Run("cancelled_context", func(t *testing.T) {
		data := buildTestTarZst(t, []testArchiveEntry{
			{name: "a.txt", data: []byte("hello")},
		})
		fsys := fstest.MapFS{
			"payload/payload.tar.zst": &fstest.MapFile{Data: data},
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Extract(ctx, fsys, "payload/payload.tar.zst", FormatTarZst, ExtractOptions{
			TargetDir: t.TempDir(),
		})
		if err == nil {
			t.Fatal("expected error for cancelled context")
		}
	})
}
