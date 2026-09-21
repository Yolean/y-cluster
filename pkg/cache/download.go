package cache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Download fetches url into dest. Cache lookups are "does the file
// exist", so dest must never hold a partial transfer: the body goes
// to a temporary file next to dest and is renamed into place only
// once it is complete. The temporary name is unique per call, so two
// provisions fetching the same artifact at once cannot interleave
// their writes; the last rename wins with a complete file either way.
func Download(ctx context.Context, url, dest string) error {
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
	if err := writeAndClose(tmp, resp.Body); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("GET %s: %w", url, err)
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
