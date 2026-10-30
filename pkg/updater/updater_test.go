package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1   string
		v2   string
		want int
	}{
		{"v0.0.7", "0.0.7", 0},
		{"v0.0.8", "v0.0.7", 1},
		{"0.0.6", "v0.0.7", -1},
		{"v1.0.0", "v0.9.9", 1},
		{"v1.0.0", "v1.0.0-rc1", 1},
		{"v1.0.0-rc1", "v1.0.0", -1},
		{"v0.10.0", "v0.2.0", 1},
	}

	for _, tc := range tests {
		got := CompareVersions(tc.v1, tc.v2)
		if got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.v1, tc.v2, got, tc.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	hash1 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hash2 := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	raw := fmt.Sprintf("# comment\n%s  TownCrier-v0.0.8-windows-amd64-setup.exe\n%s *dist/TownCrier-v0.0.8-windows-amd64.zip\n", hash1, hash2)

	m := ParseChecksums(raw)
	if m["TownCrier-v0.0.8-windows-amd64-setup.exe"] != hash1 {
		t.Errorf("unexpected hash1: %v", m)
	}
	if m["TownCrier-v0.0.8-windows-amd64.zip"] != hash2 {
		t.Errorf("unexpected hash2: %v", m)
	}
}

func TestDetectInstallModeForOS(t *testing.T) {
	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "TownCrier.exe")

	// Portable by default when no manifest or uninstall.exe exists
	if got := DetectInstallModeForOS(exePath, "windows"); got != ModePortableWindows {
		t.Fatalf("got %s, want %s", got, ModePortableWindows)
	}

	if got := DetectInstallModeForOS(exePath, "linux"); got != ModePortableUnix {
		t.Fatalf("got %s, want %s", got, ModePortableUnix)
	}

	// Installed when install-manifest.json is present
	manifestPath := filepath.Join(tmpDir, ManifestFileName)
	if err := os.WriteFile(manifestPath, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if got := DetectInstallModeForOS(exePath, "windows"); got != ModeInstalledWindows {
		t.Fatalf("got %s, want %s", got, ModeInstalledWindows)
	}
}

func TestSelectAsset(t *testing.T) {
	rel := &ReleaseInfo{
		TagName: "v0.0.8",
		Assets: []Asset{
			{Name: "TownCrier-v0.0.8-windows-amd64-setup.exe"},
			{Name: "TownCrier-v0.0.8-windows-amd64.zip"},
			{Name: "TownCrier-v0.0.8-linux-amd64.tar.gz"},
			{Name: "TownCrier-v0.0.8-darwin-arm64.tar.gz"},
			{Name: "InkAnim_v0.0.8_inkscape-extension_windows_amd64.zip"},
		},
	}

	a1, err := SelectAsset(rel, ModeInstalledWindows, "windows", "amd64")
	if err != nil || a1.Name != "TownCrier-v0.0.8-windows-amd64-setup.exe" {
		t.Fatalf("unexpected installed windows asset: %+v, err=%v", a1, err)
	}

	a2, err := SelectAsset(rel, ModePortableWindows, "windows", "amd64")
	if err != nil || a2.Name != "TownCrier-v0.0.8-windows-amd64.zip" {
		t.Fatalf("unexpected portable windows asset: %+v, err=%v", a2, err)
	}

	a3, err := SelectAsset(rel, ModePortableUnix, "linux", "amd64")
	if err != nil || a3.Name != "TownCrier-v0.0.8-linux-amd64.tar.gz" {
		t.Fatalf("unexpected linux asset: %+v, err=%v", a3, err)
	}
}

func TestCheckLatestAndDownloadVerify(t *testing.T) {
	payload := []byte("mock-setup-binary-payload-bytes")
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/mrpoundsign/TownCrier/releases/latest":
			rel := ReleaseInfo{
				TagName: "v0.0.8",
				Name:    "TownCrier v0.0.8",
				Body:    "### Release Notes\n- Added unified installer",
				Assets: []Asset{
					{
						Name:               "TownCrier-v0.0.8-windows-amd64-setup.exe",
						BrowserDownloadURL: srv.URL + "/download/TownCrier-v0.0.8-windows-amd64-setup.exe",
						Size:               int64(len(payload)),
					},
					{
						Name:               "checksums.txt",
						BrowserDownloadURL: srv.URL + "/download/checksums.txt",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(rel)
		case "/download/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  TownCrier-v0.0.8-windows-amd64-setup.exe\n", sumHex)
		case "/download/TownCrier-v0.0.8-windows-amd64-setup.exe":
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	checker := NewChecker("mrpoundsign", "TownCrier")
	checker.APIBaseURL = srv.URL

	rel, isNewer, err := checker.CheckLatest(context.Background(), "v0.0.7")
	if err != nil {
		t.Fatalf("CheckLatest failed: %v", err)
	}
	if !isNewer || rel.Version != "0.0.8" {
		t.Fatalf("expected newer version 0.0.8, got isNewer=%v version=%s", isNewer, rel.Version)
	}

	asset, err := SelectAsset(rel, ModeInstalledWindows, "windows", "amd64")
	if err != nil {
		t.Fatalf("SelectAsset failed: %v", err)
	}

	destDir := t.TempDir()
	var lastCur, lastTot int64
	verifiedPath, err := checker.DownloadAndVerify(context.Background(), rel, asset, destDir, func(cur, tot int64) {
		lastCur = cur
		lastTot = tot
	})
	if err != nil {
		t.Fatalf("DownloadAndVerify failed: %v", err)
	}
	if lastCur != int64(len(payload)) || lastTot != int64(len(payload)) {
		t.Errorf("unexpected progress: %d / %d", lastCur, lastTot)
	}

	gotBytes, err := os.ReadFile(verifiedPath)
	if err != nil || !bytes.Equal(gotBytes, payload) {
		t.Fatalf("verified file content mismatch: err=%v", err)
	}
}

func TestDownloadAndVerify_MismatchRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/checksums.txt":
			_, _ = fmt.Fprintln(w, "0000000000000000000000000000000000000000000000000000000000000000  bad.zip")
		case "/download/bad.zip":
			_, _ = w.Write([]byte("corrupted-content"))
		}
	}))
	defer srv.Close()

	checker := NewChecker("mrpoundsign", "TownCrier")
	rel := &ReleaseInfo{
		TagName: "v0.0.8",
		Assets: []Asset{
			{Name: "bad.zip", BrowserDownloadURL: srv.URL + "/download/bad.zip"},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/download/checksums.txt"},
		},
	}

	_, err := checker.DownloadAndVerify(context.Background(), rel, &rel.Assets[0], t.TempDir(), nil)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
}

func TestReplacePortableBinaryFromZipAndTarGz(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Test .zip replacement
	targetWin := filepath.Join(tmpDir, "TownCrier.exe")
	if err := os.WriteFile(targetWin, []byte("old-win-exe"), 0755); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(tmpDir, "update.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create("TownCrier.exe")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("new-win-exe"))
	_ = zw.Close()
	_ = zf.Close()

	if err := ReplacePortableBinary(zipPath, targetWin); err != nil {
		t.Fatalf("ReplacePortableBinary (.zip) failed: %v", err)
	}
	gotWin, _ := os.ReadFile(targetWin)
	if string(gotWin) != "new-win-exe" {
		t.Fatalf("expected new-win-exe, got %q", string(gotWin))
	}
	if _, err := os.Stat(targetWin + ".old"); err != nil {
		t.Fatalf("expected .old backup to exist: %v", err)
	}

	// 2. Test .tar.gz replacement
	targetLinux := filepath.Join(tmpDir, "towncrier")
	if err := os.WriteFile(targetLinux, []byte("old-linux-bin"), 0755); err != nil {
		t.Fatal(err)
	}

	tgzPath := filepath.Join(tmpDir, "update.tar.gz")
	tf, err := os.Create(tgzPath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(tf)
	tw := tar.NewWriter(gw)
	newLinuxBytes := []byte("new-linux-bin")
	_ = tw.WriteHeader(&tar.Header{
		Name:     "towncrier",
		Mode:     0755,
		Size:     int64(len(newLinuxBytes)),
		Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write(newLinuxBytes)
	_ = tw.Close()
	_ = gw.Close()
	_ = tf.Close()

	if err := ReplacePortableBinary(tgzPath, targetLinux); err != nil {
		t.Fatalf("ReplacePortableBinary (.tar.gz) failed: %v", err)
	}
	gotLinux, _ := os.ReadFile(targetLinux)
	if string(gotLinux) != "new-linux-bin" {
		t.Fatalf("expected new-linux-bin, got %q", string(gotLinux))
	}
}
