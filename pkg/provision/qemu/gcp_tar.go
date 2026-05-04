package qemu

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// writeGCPTar produces a gzip-compressed tar at outPath
// containing exactly one member named `disk.raw`. This is the
// on-the-wire shape Google Compute Engine accepts as a custom
// image source: upload to GCS, then `gcloud compute images
// create --source-uri=gs://bucket/<name>.tar.gz` ingests it
// directly. The single member name is mandated by GCE -- any
// other layout makes the image-create call fail with an
// opaque "no disk.raw found in tarball" error.
//
// Implementation detail: we'd love to pipe `qemu-img convert
// -O raw - /dev/stdout` straight into the tar/gzip stream,
// but qemu-img's raw output driver calls ftruncate() at the
// end to seal the size. ftruncate fails on a pipe, so we
// materialise the raw expansion in a temp dir alongside the
// final .tar.gz and stream from there. Tmpdir is on the
// bundle's chosen output volume (NOT /tmp), since the raw is
// the full virtual size (~20 GiB for our appliance) and /tmp
// is tmpfs on most distros.
func writeGCPTar(ctx context.Context, qcow2Src, outPath string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(outPath), ".yc-gcp-tar-")
	if err != nil {
		return fmt.Errorf("gcp-tar tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	rawPath := filepath.Join(tmpDir, "disk.raw")
	convert := exec.CommandContext(ctx, "qemu-img", "convert",
		"-f", "qcow2", "-O", "raw",
		qcow2Src, rawPath,
	)
	if out, err := convert.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert (gcp-tar): %s: %w", out, err)
	}

	rawInfo, err := os.Stat(rawPath)
	if err != nil {
		return fmt.Errorf("stat raw: %w", err)
	}

	rawFile, err := os.Open(rawPath)
	if err != nil {
		return fmt.Errorf("open raw: %w", err)
	}
	defer rawFile.Close()

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create gcp tar: %w", err)
	}
	defer out.Close()

	gzw, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("gzip writer: %w", err)
	}
	tw := tar.NewWriter(gzw)

	// Force GNU tar format. For files >8 GiB (our 20 GiB
	// raw is well above this), the default tar format in
	// Go's archive/tar emits a PAX extended-header tarball
	// member ahead of the actual file. GCE's image parser
	// expects a SINGLE member literally named `disk.raw`
	// and treats the PaxHeaders entry as "not disk.raw",
	// rejecting the upload with the unhelpful "The tar
	// archive is not a valid image." GNU format encodes
	// large sizes inline in the header (base-256 numeric
	// fields) -- no separate member, no rejection.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "disk.raw",
		Mode:     0o644,
		Size:     rawInfo.Size(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatGNU,
	}); err != nil {
		_ = tw.Close()
		_ = gzw.Close()
		return fmt.Errorf("tar header: %w", err)
	}

	if _, err := io.Copy(tw, rawFile); err != nil {
		_ = tw.Close()
		_ = gzw.Close()
		return fmt.Errorf("stream raw -> tar: %w", err)
	}

	if err := tw.Close(); err != nil {
		_ = gzw.Close()
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	return nil
}
