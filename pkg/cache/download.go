package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Download fetches url into dest. Cache lookups are "does the file
// exist", so dest must never hold a partial transfer: the body goes
// to a temporary file next to dest and is renamed into place only
// once it is complete. The temporary name is unique per call, so two
// provisions fetching the same artifact at once cannot interleave
// their writes; the last rename wins with a complete file either way.
func Download(ctx context.Context, url, dest string) error {
	return download(ctx, url, dest, "")
}

// DownloadSHA256 is Download for an artifact whose digest is known
// in advance: the body is hashed as it is written, and dest is only
// created when the SHA-256 is wantHex. A mismatch leaves no file.
func DownloadSHA256(ctx context.Context, url, dest, wantHex string) error {
	if len(wantHex) != sha256.Size*2 {
		return fmt.Errorf("GET %s: expected a SHA-256 of %d hex characters, got %q", url, sha256.Size*2, wantHex)
	}
	return download(ctx, url, dest, strings.ToLower(wantHex))
}

func download(ctx context.Context, url, dest, wantSHA256 string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".*.tmp")
	if err != nil {
		return err
	}
	h := sha256.New()
	if err := writeAndClose(tmp, io.TeeReader(resp.Body, h)); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("GET %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); wantSHA256 != "" && got != wantSHA256 {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("GET %s: SHA-256 is %s, want %s", url, got, wantSHA256)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

func writeAndClose(f *os.File, body io.Reader) error {
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return err
	}
	// CreateTemp makes the file 0600; cache entries are plain
	// downloads that other tools of the same host may read.
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
