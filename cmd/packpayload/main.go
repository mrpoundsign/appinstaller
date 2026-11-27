package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/mrpoundsign/appinstaller/internal/payloadarchive"
)

type packEntry struct {
	srcPath     string
	archivePath string
	size        int64
	mode        int64
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "packpayload: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fsFlags := flag.NewFlagSet("packpayload", flag.ContinueOnError)
	fsFlags.SetOutput(io.Discard)

	outPath := fsFlags.String("out", "", "output archive path (required)")
	srcDir := fsFlags.String("src", "", "directory to pack recursively")
	formatFlag := fsFlags.String("format", "auto", "archive format: auto, tar.zst, tar.gz, or zip")
	levelFlag := fsFlags.Int("level", 0, "compression level (default: 19 for tar.zst, 9 for tar.gz/zip)")
	goZstd := fsFlags.Bool("go-zstd", false, "force pure-Go zstd encoder even if zstd CLI is installed")

	if err := fsFlags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*outPath) == "" {
		return errors.New("-out is required")
	}

	format, err := resolveFormat(*formatFlag, *outPath)
	if err != nil {
		return err
	}
	level := resolveLevel(format, *levelFlag)

	entries, totalRaw, err := collectEntries(*srcDir, fsFlags.Args())
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("no payload files found to pack")
	}

	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	tmpOut := *outPath + ".tmp"
	_ = os.Remove(tmpOut)
	defer func() {
		_ = os.Remove(tmpOut)
	}()

	switch format {
	case payloadarchive.FormatTarZst:
		if err := packTarZst(tmpOut, entries, level, *goZstd); err != nil {
			return err
		}
	case payloadarchive.FormatTarGz:
		if err := packTarGz(tmpOut, entries, level); err != nil {
			return err
		}
	case payloadarchive.FormatZip:
		if err := packZip(tmpOut, entries, level); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported format %q", format)
	}

	info, err := os.Stat(tmpOut)
	if err != nil {
		return fmt.Errorf("stat packed archive: %w", err)
	}
	_ = os.Remove(*outPath)
	if err := os.Rename(tmpOut, *outPath); err != nil {
		return fmt.Errorf("finalize output archive: %w", err)
	}

	if stdout != nil {
		pct := 0.0
		if totalRaw > 0 {
			pct = (1.0 - float64(info.Size())/float64(totalRaw)) * 100.0
		}
		_, _ = fmt.Fprintf(stdout, "Packed %d files into %s (%s): %.2f MB -> %.2f MB (%.1f%% smaller)\n",
			len(entries), filepath.Base(*outPath), format,
			float64(totalRaw)/(1024*1024), float64(info.Size())/(1024*1024), pct)
	}
	return nil
}

func resolveFormat(formatFlag, outPath string) (payloadarchive.Format, error) {
	switch strings.ToLower(strings.TrimSpace(formatFlag)) {
	case "", "auto":
		lower := strings.ToLower(outPath)
		switch {
		case strings.HasSuffix(lower, ".tar.zst"), strings.HasSuffix(lower, ".tzst"):
			return payloadarchive.FormatTarZst, nil
		case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
			return payloadarchive.FormatTarGz, nil
		case strings.HasSuffix(lower, ".zip"):
			return payloadarchive.FormatZip, nil
		default:
			return "", fmt.Errorf("cannot infer archive format from %q; specify -format", outPath)
		}
	case "tar.zst", "zst":
		return payloadarchive.FormatTarZst, nil
	case "tar.gz", "gz":
		return payloadarchive.FormatTarGz, nil
	case "zip":
		return payloadarchive.FormatZip, nil
	default:
		return "", fmt.Errorf("unsupported -format %q", formatFlag)
	}
}

func resolveLevel(format payloadarchive.Format, levelFlag int) int {
	if levelFlag > 0 {
		return levelFlag
	}
	if format == payloadarchive.FormatTarZst {
		return 19
	}
	return 9
}

func collectEntries(srcDir string, positional []string) ([]packEntry, int64, error) {
	byPath := make(map[string]packEntry)

	if strings.TrimSpace(srcDir) != "" {
		cleanSrc := filepath.Clean(srcDir)
		err := filepath.WalkDir(cleanSrc, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			relOS, err := filepath.Rel(cleanSrc, p)
			if err != nil {
				return err
			}
			relSlash := filepath.ToSlash(relOS)
			if shouldSkipRelative(relSlash) {
				return nil
			}
			entry, err := makePackEntry(p, relSlash)
			if err != nil {
				return err
			}
			byPath[entry.archivePath] = entry
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("walk %s: %w", srcDir, err)
		}
	}

	for _, arg := range positional {
		srcPath, destRel, hasMapping := strings.Cut(arg, "=")
		srcPath = strings.TrimSpace(srcPath)
		if srcPath == "" {
			return nil, 0, fmt.Errorf("invalid positional argument %q", arg)
		}
		if !hasMapping {
			destRel = filepath.Base(srcPath)
		}
		entry, err := makePackEntry(srcPath, destRel)
		if err != nil {
			return nil, 0, err
		}
		byPath[entry.archivePath] = entry
	}

	entries := make([]packEntry, 0, len(byPath))
	var totalRaw int64
	for _, e := range byPath {
		entries = append(entries, e)
		totalRaw += e.size
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].archivePath < entries[j].archivePath
	})
	return entries, totalRaw, nil
}

func shouldSkipRelative(relSlash string) bool {
	base := path.Base(relSlash)
	if relSlash == "placeholder.txt" || base == ".DS_Store" || base == "Thumbs.db" {
		return true
	}
	if base == "payload.zip" || strings.HasPrefix(base, "payload.tar.") {
		return true
	}
	return false
}

func makePackEntry(srcPath, destRel string) (packEntry, error) {
	info, err := os.Stat(srcPath)
	if err != nil {
		return packEntry{}, fmt.Errorf("stat %s: %w", srcPath, err)
	}
	if info.IsDir() {
		return packEntry{}, fmt.Errorf("source %s is a directory; use -src for directories", srcPath)
	}
	cleanDest, err := payloadarchive.SanitizeArchivePath(destRel)
	if err != nil {
		return packEntry{}, err
	}
	return packEntry{
		srcPath:     srcPath,
		archivePath: cleanDest,
		size:        info.Size(),
		mode:        entryFileMode(cleanDest, info.Mode()),
	}, nil
}

func entryFileMode(archivePath string, srcMode fs.FileMode) int64 {
	if srcMode&0o111 != 0 {
		return 0o755
	}
	switch strings.ToLower(path.Ext(archivePath)) {
	case ".exe", ".dll", ".so", ".dylib", ".sh":
		return 0o755
	default:
		return 0o644
	}
}

func writeTarStream(w io.Writer, entries []packEntry) error {
	tw := tar.NewWriter(w)
	fixedTime := time.Unix(0, 0).UTC()

	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.archivePath,
			Typeflag: tar.TypeReg,
			Size:     e.size,
			Mode:     e.mode,
			ModTime:  fixedTime,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			return fmt.Errorf("write tar header %s: %w", e.archivePath, err)
		}
		f, err := os.Open(e.srcPath)
		if err != nil {
			_ = tw.Close()
			return fmt.Errorf("open %s: %w", e.srcPath, err)
		}
		n, copyErr := io.Copy(tw, f)
		_ = f.Close()
		if copyErr != nil {
			_ = tw.Close()
			return fmt.Errorf("write tar body %s: %w", e.archivePath, copyErr)
		}
		if n != e.size {
			_ = tw.Close()
			return fmt.Errorf("size mismatch for %s: wrote %d, want %d", e.archivePath, n, e.size)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar stream: %w", err)
	}
	return nil
}

func packTarZst(outPath string, entries []packEntry, level int, forceGo bool) error {
	if !forceGo {
		if zstdBin, err := exec.LookPath("zstd"); err == nil {
			return packTarZstCLI(zstdBin, outPath, entries, level)
		}
	}

	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}

	zw, err := zstd.NewWriter(outFile, zstd.WithEncoderLevel(mapZstdLevel(level)))
	if err != nil {
		_ = outFile.Close()
		return fmt.Errorf("init zstd writer: %w", err)
	}

	writeErr := writeTarStream(zw, entries)
	closeZstdErr := zw.Close()
	closeFileErr := outFile.Close()

	if writeErr != nil {
		return writeErr
	}
	if closeZstdErr != nil {
		return fmt.Errorf("finalize zstd stream: %w", closeZstdErr)
	}
	if closeFileErr != nil {
		return fmt.Errorf("close %s: %w", outPath, closeFileErr)
	}
	return nil
}

func packTarZstCLI(zstdBin, outPath string, entries []packEntry, level int) error {
	cliArgs := make([]string, 0, 7)
	if level > 19 {
		cliArgs = append(cliArgs, "--ultra")
	}
	cliArgs = append(cliArgs, "-T0", fmt.Sprintf("-%d", level), "-q", "-f", "-o", outPath, "-")

	cmd := exec.Command(zstdBin, cliArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("zstd stdin pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start zstd CLI: %w", err)
	}

	tarErr := writeTarStream(stdin, entries)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()

	if tarErr != nil {
		return tarErr
	}
	if closeErr != nil {
		return fmt.Errorf("close zstd stdin: %w", closeErr)
	}
	if waitErr != nil {
		return fmt.Errorf("zstd CLI failed: %w (%s)", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func mapZstdLevel(level int) zstd.EncoderLevel {
	switch {
	case level >= 15:
		return zstd.SpeedBestCompression
	case level >= 8:
		return zstd.SpeedBetterCompression
	case level >= 3:
		return zstd.SpeedDefault
	default:
		return zstd.SpeedFastest
	}
}

func packTarGz(outPath string, entries []packEntry, level int) error {
	if level < gzip.BestSpeed || level > gzip.BestCompression {
		level = gzip.BestCompression
	}
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}

	gw, err := gzip.NewWriterLevel(outFile, level)
	if err != nil {
		_ = outFile.Close()
		return fmt.Errorf("init gzip writer: %w", err)
	}

	writeErr := writeTarStream(gw, entries)
	closeGzErr := gw.Close()
	closeFileErr := outFile.Close()

	if writeErr != nil {
		return writeErr
	}
	if closeGzErr != nil {
		return fmt.Errorf("finalize gzip stream: %w", closeGzErr)
	}
	if closeFileErr != nil {
		return fmt.Errorf("close %s: %w", outPath, closeFileErr)
	}
	return nil
}

func packZip(outPath string, entries []packEntry, level int) error {
	if level < flate.BestSpeed || level > flate.BestCompression {
		level = flate.BestCompression
	}
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}

	zw := zip.NewWriter(outFile)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, level)
	})

	fixedTime := time.Unix(0, 0).UTC()
	for _, e := range entries {
		hdr := &zip.FileHeader{
			Name:     e.archivePath,
			Method:   zip.Deflate,
			Modified: fixedTime,
		}
		hdr.SetMode(fs.FileMode(e.mode))
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			_ = zw.Close()
			_ = outFile.Close()
			return fmt.Errorf("create zip entry %s: %w", e.archivePath, err)
		}
		f, err := os.Open(e.srcPath)
		if err != nil {
			_ = zw.Close()
			_ = outFile.Close()
			return fmt.Errorf("open %s: %w", e.srcPath, err)
		}
		_, copyErr := io.Copy(w, f)
		_ = f.Close()
		if copyErr != nil {
			_ = zw.Close()
			_ = outFile.Close()
			return fmt.Errorf("write zip entry %s: %w", e.archivePath, copyErr)
		}
	}

	closeZipErr := zw.Close()
	closeFileErr := outFile.Close()
	if closeZipErr != nil {
		return fmt.Errorf("finalize zip %s: %w", outPath, closeZipErr)
	}
	if closeFileErr != nil {
		return fmt.Errorf("close %s: %w", outPath, closeFileErr)
	}
	return nil
}
