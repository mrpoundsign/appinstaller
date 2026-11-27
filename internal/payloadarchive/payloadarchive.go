package payloadarchive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

// Format identifies a supported compressed payload archive format.
type Format string

const (
	FormatTarZst Format = "tar.zst"
	FormatTarGz  Format = "tar.gz"
	FormatZip    Format = "zip"
)

var candidateArchives = []struct {
	name   string
	format Format
}{
	{name: "payload.tar.zst", format: FormatTarZst},
	{name: "payload.tar.gz", format: FormatTarGz},
	{name: "payload.zip", format: FormatZip},
}

// ExtractOptions configures extraction from an embedded payload archive.
type ExtractOptions struct {
	TargetDir   string
	FileRenames map[string]string
	OnFileStart func(dstRel string, bytesRead, totalCompressed int64)
}

// ExtractResult summarizes the files written by Extract.
type ExtractResult struct {
	Files      []string
	TotalBytes int64
}

// DetectArchive checks payloadFS/subdir for a known compressed payload archive
// in priority order: payload.tar.zst, payload.tar.gz, payload.zip.
func DetectArchive(payloadFS fs.FS, subdir string) (string, Format, bool) {
	if payloadFS == nil {
		return "", "", false
	}
	cleanSub := strings.Trim(path.Clean(strings.ReplaceAll(subdir, "\\", "/")), "/")
	if cleanSub == "." {
		cleanSub = ""
	}
	for _, cand := range candidateArchives {
		candidatePath := cand.name
		if cleanSub != "" {
			candidatePath = cleanSub + "/" + cand.name
		}
		info, err := fs.Stat(payloadFS, candidatePath)
		if err == nil && !info.IsDir() {
			return candidatePath, cand.format, true
		}
	}
	return "", "", false
}

// HashFile computes the SHA-256 hex digest and byte length of embeddedPath in payloadFS.
func HashFile(payloadFS fs.FS, embeddedPath string) (string, int64, error) {
	f, err := payloadFS.Open(embeddedPath)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", embeddedPath, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", embeddedPath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SanitizeArchivePath validates and normalizes an archive-relative slash path,
// rejecting absolute paths, drive prefixes, NUL bytes, and parent directory traversal.
func SanitizeArchivePath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("invalid archive path %q: contains NUL byte", raw)
	}
	norm := strings.ReplaceAll(raw, "\\", "/")
	norm = strings.TrimPrefix(norm, "./")
	if norm == "" || norm == "." {
		return "", fmt.Errorf("invalid archive path %q: empty path", raw)
	}
	if strings.HasPrefix(norm, "/") {
		return "", fmt.Errorf("invalid archive path %q: absolute path not allowed", raw)
	}
	if len(norm) >= 2 && norm[1] == ':' && ((norm[0] >= 'a' && norm[0] <= 'z') || (norm[0] >= 'A' && norm[0] <= 'Z')) {
		return "", fmt.Errorf("invalid archive path %q: drive prefix not allowed", raw)
	}
	for seg := range strings.SplitSeq(norm, "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid archive path %q: path traversal not allowed", raw)
		}
	}
	cleaned := path.Clean(norm)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("invalid archive path %q: path traversal not allowed", raw)
	}
	return cleaned, nil
}

// ResolveTargetPath validates rel and joins it with targetDir, ensuring the result stays inside targetDir.
func ResolveTargetPath(targetDir, rel string) (string, error) {
	cleanRel, err := SanitizeArchivePath(rel)
	if err != nil {
		return "", err
	}
	cleanRoot := filepath.Clean(targetDir)
	dst := filepath.Clean(filepath.Join(cleanRoot, filepath.FromSlash(cleanRel)))
	relCheck, err := filepath.Rel(cleanRoot, dst)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) || filepath.IsAbs(relCheck) {
		return "", fmt.Errorf("archive path %q escapes target directory %q", rel, targetDir)
	}
	return dst, nil
}

// ApplyRename maps rel according to renames, matching full relative path first and basename second.
func ApplyRename(rel string, renames map[string]string) string {
	if len(renames) == 0 {
		return rel
	}
	if mapped, ok := renames[rel]; ok && mapped != "" {
		if sanitized, err := SanitizeArchivePath(mapped); err == nil {
			return sanitized
		}
		return mapped
	}
	base := path.Base(rel)
	if mappedBase, ok := renames[base]; ok && mappedBase != "" {
		dir := path.Dir(rel)
		joined := mappedBase
		if dir != "." && dir != "" {
			joined = path.Join(dir, mappedBase)
		}
		if sanitized, err := SanitizeArchivePath(joined); err == nil {
			return sanitized
		}
		return joined
	}
	return rel
}

// Extract unpacks an archive from payloadFS into opts.TargetDir with zip-slip validation and atomic writes.
func Extract(ctx context.Context, payloadFS fs.FS, embeddedPath string, format Format, opts ExtractOptions) (*ExtractResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.TargetDir == "" {
		return nil, errors.New("target directory is required")
	}

	switch format {
	case FormatTarZst, FormatTarGz:
		return extractTar(ctx, payloadFS, embeddedPath, format, opts)
	case FormatZip:
		return extractZip(ctx, payloadFS, embeddedPath, opts)
	default:
		return nil, fmt.Errorf("unsupported archive format %q", format)
	}
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

func (c *countingReader) BytesRead() int64 {
	return c.n.Load()
}

func extractTar(ctx context.Context, payloadFS fs.FS, embeddedPath string, format Format, opts ExtractOptions) (*ExtractResult, error) {
	f, err := payloadFS.Open(embeddedPath)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %w", embeddedPath, err)
	}
	defer func() { _ = f.Close() }()

	var totalCompressed int64
	if info, statErr := f.Stat(); statErr == nil {
		totalCompressed = info.Size()
	}

	cr := &countingReader{r: f}

	var tarStream io.Reader
	switch format {
	case FormatTarZst:
		dec, zerr := zstd.NewReader(cr)
		if zerr != nil {
			return nil, fmt.Errorf("init zstd reader for %s: %w", embeddedPath, zerr)
		}
		defer dec.Close()
		tarStream = dec
	case FormatTarGz:
		gz, gerr := gzip.NewReader(cr)
		if gerr != nil {
			return nil, fmt.Errorf("init gzip reader for %s: %w", embeddedPath, gerr)
		}
		defer func() { _ = gz.Close() }()
		tarStream = gz
	default:
		return nil, fmt.Errorf("unexpected tar format %q", format)
	}

	tr := tar.NewReader(tarStream)
	res := &ExtractResult{}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry from %s: %w", embeddedPath, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if _, serr := SanitizeArchivePath(strings.TrimSuffix(hdr.Name, "/")); serr != nil && hdr.Name != "." && hdr.Name != "./" {
				return nil, serr
			}
			continue
		case tar.TypeReg:
			// regular file handled below
		case tar.TypeSymlink, tar.TypeLink:
			return nil, fmt.Errorf("archive entry %q uses disallowed link type %q", hdr.Name, hdr.Typeflag)
		default:
			return nil, fmt.Errorf("archive entry %q uses unsupported type %q", hdr.Name, hdr.Typeflag)
		}

		rel, err := SanitizeArchivePath(hdr.Name)
		if err != nil {
			return nil, err
		}
		if rel == "placeholder.txt" {
			continue
		}

		dstRel := ApplyRename(rel, opts.FileRenames)
		dstFull, err := ResolveTargetPath(opts.TargetDir, dstRel)
		if err != nil {
			return nil, err
		}
		cleanDstRel, err := SanitizeArchivePath(dstRel)
		if err != nil {
			return nil, err
		}

		if opts.OnFileStart != nil {
			opts.OnFileStart(cleanDstRel, cr.BytesRead(), totalCompressed)
		}

		n, err := writeAtomic(dstFull, tr)
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", cleanDstRel, err)
		}
		res.TotalBytes += n
		res.Files = append(res.Files, cleanDstRel)
	}

	return res, nil
}

func extractZip(ctx context.Context, payloadFS fs.FS, embeddedPath string, opts ExtractOptions) (*ExtractResult, error) {
	f, err := payloadFS.Open(embeddedPath)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %w", embeddedPath, err)
	}
	defer func() { _ = f.Close() }()

	var (
		ra   io.ReaderAt
		size int64
	)
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	if rAt, ok := f.(io.ReaderAt); ok && size > 0 {
		ra = rAt
	} else {
		data, readErr := io.ReadAll(f)
		if readErr != nil {
			return nil, fmt.Errorf("read zip archive %s: %w", embeddedPath, readErr)
		}
		ra = bytes.NewReader(data)
		size = int64(len(data))
	}

	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("open zip reader for %s: %w", embeddedPath, err)
	}

	var totalCompressed int64
	for _, zf := range zr.File {
		totalCompressed += int64(zf.CompressedSize64)
	}

	res := &ExtractResult{}
	var bytesRead int64

	for _, zf := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if zf.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("archive entry %q is a disallowed symlink", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if _, serr := SanitizeArchivePath(strings.TrimSuffix(zf.Name, "/")); serr != nil && zf.Name != "." && zf.Name != "./" {
				return nil, serr
			}
			continue
		}

		rel, err := SanitizeArchivePath(zf.Name)
		if err != nil {
			return nil, err
		}
		if rel == "placeholder.txt" {
			bytesRead += int64(zf.CompressedSize64)
			continue
		}

		dstRel := ApplyRename(rel, opts.FileRenames)
		dstFull, err := ResolveTargetPath(opts.TargetDir, dstRel)
		if err != nil {
			return nil, err
		}
		cleanDstRel, err := SanitizeArchivePath(dstRel)
		if err != nil {
			return nil, err
		}

		if opts.OnFileStart != nil {
			opts.OnFileStart(cleanDstRel, bytesRead, totalCompressed)
		}

		rc, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %s: %w", cleanDstRel, err)
		}
		n, writeErr := writeAtomic(dstFull, rc)
		closeErr := rc.Close()
		if writeErr != nil {
			return nil, fmt.Errorf("extract %s: %w", cleanDstRel, writeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close zip entry %s: %w", cleanDstRel, closeErr)
		}

		bytesRead += int64(zf.CompressedSize64)
		res.TotalBytes += n
		res.Files = append(res.Files, cleanDstRel)
	}

	return res, nil
}

func writeAtomic(dstPath string, src io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, err
	}

	tmpPath := dstPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return 0, err
	}

	n, copyErr := io.Copy(dst, src)
	if closeErr := dst.Close(); closeErr != nil && copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return 0, copyErr
	}

	_ = os.Remove(dstPath)
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	return n, nil
}
