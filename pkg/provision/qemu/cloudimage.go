package qemu

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/cache"
)

// ubuntuCloudImageKeyring holds Ubuntu's cloud image signing keys,
// exported in armored form from /usr/share/keyrings/
// ubuntu-cloudimage-keyring.gpg of the ubuntu-keyring package
// (2023.11.28.1, Ubuntu 24.04). The fingerprints are pinned in
// cloudimage_test.go, so a change to this file is a visible decision.
//
//go:embed ubuntu-cloudimage-keyring.asc
var ubuntuCloudImageKeyring []byte

// cloudImageBaseURL is the directory EnsureCloudImage downloads from,
// with the release's SHA256SUMS and SHA256SUMS.gpg next to the image.
const cloudImageBaseURL = "https://cloud-images.ubuntu.com/" + ubuntuVersion + "/current/"

// cloudImageFile is the image's name in SHA256SUMS.
const cloudImageFile = ubuntuVersion + "-server-cloudimg-amd64.img"

// smallFileLimit bounds what fetchSmall reads: SHA256SUMS is a few
// kilobytes and its signature under one.
const smallFileLimit = 1 << 20

// VerifiedCloudImage is a cloud image whose SHA-256 is the one in the
// release's signed SHA256SUMS.
type VerifiedCloudImage struct {
	Path   string
	SHA256 string
	URL    string
	// SignedBy is the fingerprint of the key that signed SHA256SUMS,
	// and SignedAt the signature's creation time.
	SignedBy string
	SignedAt time.Time
}

// EnsureVerifiedCloudImage returns the newest Ubuntu cloud image,
// verified, in dir. It is for guests that are recreated on a cycle and
// must start from the image the release publishes today, where
// EnsureCloudImage keeps whatever it downloaded first for as long as
// disks are backed by it.
//
// Every call fetches SHA256SUMS and its detached OpenPGP signature,
// checks the signature against ubuntuCloudImageKeyring and looks up
// the image's digest in the signed file. The image is stored under
// that digest, so a newer image never replaces a file an existing
// disk is backed by, and a file already in dir is used again only
// when its content still hashes to the signed digest.
func EnsureVerifiedCloudImage(ctx context.Context, dir string, logger *zap.Logger) (VerifiedCloudImage, error) {
	return ensureVerifiedCloudImage(ctx, cloudImageBaseURL, dir, ubuntuCloudImageKeyring, logger)
}

func ensureVerifiedCloudImage(ctx context.Context, baseURL, dir string, armoredKeyring []byte, logger *zap.Logger) (VerifiedCloudImage, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	sums, err := fetchSmall(ctx, baseURL+"SHA256SUMS")
	if err != nil {
		return VerifiedCloudImage{}, err
	}
	sig, err := fetchSmall(ctx, baseURL+"SHA256SUMS.gpg")
	if err != nil {
		return VerifiedCloudImage{}, err
	}
	img := VerifiedCloudImage{URL: baseURL + cloudImageFile}
	img.SignedBy, img.SignedAt, err = verifyDetachedSignature(armoredKeyring, sums, sig)
	if err != nil {
		return VerifiedCloudImage{}, fmt.Errorf("%sSHA256SUMS: %w", baseURL, err)
	}
	img.SHA256, err = signedDigest(sums, cloudImageFile)
	if err != nil {
		return VerifiedCloudImage{}, fmt.Errorf("%sSHA256SUMS: %w", baseURL, err)
	}
	img.Path = filepath.Join(dir, fmt.Sprintf("ubuntu-%s-%s.img", strings.TrimSuffix(cloudImageFile, ".img"), img.SHA256[:16]))

	if got, err := fileSHA256(img.Path); err == nil && got == img.SHA256 {
		logger.Debug("verified cloud image already present", zap.String("path", img.Path))
		return img, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return VerifiedCloudImage{}, err
	}
	logger.Info("downloading cloud image",
		zap.String("url", img.URL), zap.String("sha256", img.SHA256),
		zap.Time("signedAt", img.SignedAt))
	if err := cache.DownloadSHA256(ctx, img.URL, img.Path, img.SHA256); err != nil {
		return VerifiedCloudImage{}, fmt.Errorf("download cloud image: %w", err)
	}
	return img, nil
}

// verifyDetachedSignature checks sig over signed against the armored
// keyring and returns the signer's fingerprint and the signature time.
func verifyDetachedSignature(armoredKeyring, signed, sig []byte) (string, time.Time, error) {
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armoredKeyring))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read signing keys: %w", err)
	}
	var sigReader io.Reader = bytes.NewReader(sig)
	// Ubuntu publishes SHA256SUMS.gpg armored; a binary signature is
	// accepted too.
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN PGP SIGNATURE-----")) {
		block, err := armor.Decode(bytes.NewReader(sig))
		if err != nil {
			return "", time.Time{}, fmt.Errorf("decode armored signature: %w", err)
		}
		sigReader = block.Body
	}
	s, signer, err := openpgp.VerifyDetachedSignature(keyring, bytes.NewReader(signed), sigReader, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signature does not verify against Ubuntu's cloud image keys: %w", err)
	}
	return strings.ToUpper(hex.EncodeToString(signer.PrimaryKey.Fingerprint)), s.CreationTime, nil
}

// signedDigest returns the SHA-256 SHA256SUMS lists for name. Lines
// are `<hex> *<name>` (binary mode) or `<hex>  <name>`.
func signedDigest(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest := strings.ToLower(fields[0])
		if b, err := hex.DecodeString(digest); err != nil || len(b) != sha256.Size {
			return "", fmt.Errorf("malformed digest %q for %s", fields[0], name)
		}
		return digest, nil
	}
	return "", fmt.Errorf("no entry for %s", name)
}

func fetchSmall(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, smallFileLimit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if len(body) > smallFileLimit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, smallFileLimit)
	}
	return body, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PruneCloudImages removes the verified images in dir other than keep.
// Only for a directory whose images no disk outside the caller's
// control is backed by.
func PruneCloudImages(dir, keep string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "ubuntu-"+strings.TrimSuffix(cloudImageFile, ".img")+"-*.img"))
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, m := range matches {
		if m == keep {
			continue
		}
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed = append(removed, m)
	}
	return removed, nil
}
