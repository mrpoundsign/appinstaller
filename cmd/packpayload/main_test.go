package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mrpoundsign/appinstaller/internal/payloadarchive"
)

func TestPackPayloadFormatsAndRoundTrip(t *testing.T) {
	tmpRoot := t.TempDir()
	srcDir := filepath.Join(tmpRoot, "staged")
	if err := os.MkdirAll(filepath.Join(srcDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "placeholder.txt"), []byte("skip-me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "app.exe"), []byte("exe-binary-payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "docs", "guide.md"), []byte("# Guide"), 0o644); err != nil {
		t.Fatal(err)
	}

	extraFile := filepath.Join(tmpRoot, "extra-license.txt")
	if err := os.WriteFile(extraFile, []byte("AGPL-3.0"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		outName string
		format  payloadarchive.Format
		extra   []string
	}{
		{name: "tar_zst_default", outName: "payload.tar.zst", format: payloadarchive.FormatTarZst},
		{name: "tar_zst_go", outName: "payload.tar.zst", format: payloadarchive.FormatTarZst, extra: []string{"-go-zstd"}},
		{name: "tar_gz", outName: "payload.tar.gz", format: payloadarchive.FormatTarGz},
		{name: "zip", outName: "payload.zip", format: payloadarchive.FormatZip},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outSubdir := filepath.Join(tmpRoot, tc.name, "payload")
			outPath := filepath.Join(outSubdir, tc.outName)

			args := append([]string{}, tc.extra...)
			args = append(args, "-src", srcDir, "-out", outPath, extraFile+"=LEGAL/LICENSE")

			var stdout bytes.Buffer
			if err := run(args, &stdout); err != nil {
				t.Fatalf("run(%v) failed: %v", args, err)
			}
			if stdout.Len() == 0 {
				t.Error("expected summary line on stdout")
			}

			extractDir := filepath.Join(tmpRoot, tc.name, "extracted")
			fsys := os.DirFS(filepath.Dir(outSubdir))
			embPath, detectedFmt, ok := payloadarchive.DetectArchive(fsys, "payload")
			if !ok || detectedFmt != tc.format {
				t.Fatalf("DetectArchive got ok=%v fmt=%q, want %q", ok, detectedFmt, tc.format)
			}

			res, err := payloadarchive.Extract(context.Background(), fsys, embPath, detectedFmt, payloadarchive.ExtractOptions{
				TargetDir: extractDir,
			})
			if err != nil {
				t.Fatalf("Extract failed: %v", err)
			}

			wantFiles := []string{"LEGAL/LICENSE", "app.exe", "docs/guide.md"}
			if !slices.Equal(res.Files, wantFiles) {
				t.Errorf("extracted files = %v, want %v", res.Files, wantFiles)
			}

			gotLicense, err := os.ReadFile(filepath.Join(extractDir, "LEGAL", "LICENSE"))
			if err != nil || string(gotLicense) != "AGPL-3.0" {
				t.Errorf("unexpected LEGAL/LICENSE: %q, err=%v", gotLicense, err)
			}
		})
	}
}
