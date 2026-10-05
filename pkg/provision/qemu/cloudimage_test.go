package qemu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// The embedded keyring is the trust anchor for every cloud image a
// dockerhost guest boots; changing it must be a deliberate edit here.
func TestUbuntuCloudImageKeyring_Fingerprints(t *testing.T) {
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(ubuntuCloudImageKeyring))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range keyring {
		got = append(got, strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)))
	}
	sort.Strings(got)
	want := []string{
		"4A3CE3CD565D7EB5C810E2B97FF3F408476CF100", // Ubuntu Cloud Image Builder
		"D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81", // UEC Image Automatic Signing Key
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keyring fingerprints %v, want %v", got, want)
	}
}

// testdata/cloudimage holds noble/current's SHA256SUMS and its
// signature as published on 2026-10-05.
func TestVerifyDetachedSignature_PublishedRelease(t *testing.T) {
	sums, sig := readFixture(t, "SHA256SUMS"), readFixture(t, "SHA256SUMS.gpg")
	signer, at, err := verifyDetachedSignature(ubuntuCloudImageKeyring, sums, sig)
	if err != nil {
		t.Fatal(err)
	}
	if signer != "D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81" {
		t.Errorf("signer %s", signer)
	}
	if want := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC); at.Before(want) || at.After(want.Add(24*time.Hour)) {
		t.Errorf("signed at %s, want 2026-09-26", at)
	}
	digest, err := signedDigest(sums, cloudImageFile)
	if err != nil {
		t.Fatal(err)
	}
	if digest != "6a81c37564db9b1ee84e141922625e1d7c5b389b99bb3c572e0243607d5bb4d2" {
		t.Errorf("digest %s", digest)
	}

	tampered := bytes.Replace(sums, []byte("6a81c375"), []byte("6a81c376"), 1)
	if _, _, err := verifyDetachedSignature(ubuntuCloudImageKeyring, tampered, sig); err == nil {
		t.Fatal("a SHA256SUMS changed after signing must not verify")
	}
}

func TestVerifyDetachedSignature_RejectsOtherSigner(t *testing.T) {
	sums := readFixture(t, "SHA256SUMS")
	_, sig := testSigner(t, sums)
	if _, _, err := verifyDetachedSignature(ubuntuCloudImageKeyring, sums, sig); err == nil {
		t.Fatal("a signature by a key outside Ubuntu's keyring must not verify")
	}
}

func TestSignedDigest(t *testing.T) {
	d := strings.Repeat("ab", 32)
	for _, line := range []string{d + " *" + cloudImageFile, d + "  " + cloudImageFile} {
		got, err := signedDigest([]byte("ffff *other.img\n"+line+"\n"), cloudImageFile)
		if err != nil || got != d {
			t.Errorf("%q: got %q, %v", line, got, err)
		}
	}
	if _, err := signedDigest([]byte(d+" *other.img\n"), cloudImageFile); err == nil {
		t.Error("a missing entry must be an error")
	}
	if _, err := signedDigest([]byte("zz *"+cloudImageFile+"\n"), cloudImageFile); err == nil {
		t.Error("a malformed digest must be an error")
	}
}

func TestEnsureVerifiedCloudImage_DownloadsVerifiesAndReuses(t *testing.T) {
	image := []byte("not really a disk image\n")
	sum := sha256.Sum256(image)
	sums := []byte(fmt.Sprintf("%s *%s\n", hex.EncodeToString(sum[:]), cloudImageFile))
	keyring, sig := testSigner(t, sums)

	var imageGets atomic.Int32
	serve := image
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			_, _ = w.Write(sums)
		case "/SHA256SUMS.gpg":
			_, _ = w.Write(sig)
		case "/" + cloudImageFile:
			imageGets.Add(1)
			_, _ = w.Write(serve)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	img, err := ensureVerifiedCloudImage(context.Background(), srv.URL+"/", dir, keyring, nil)
	if err != nil {
		t.Fatal(err)
	}
	if img.SHA256 != hex.EncodeToString(sum[:]) || filepath.Dir(img.Path) != dir {
		t.Fatalf("unexpected image %+v", img)
	}
	if !strings.Contains(filepath.Base(img.Path), img.SHA256[:16]) {
		t.Errorf("image file %s is not named by its digest", img.Path)
	}
	if got, _ := os.ReadFile(img.Path); !bytes.Equal(got, image) {
		t.Fatal("image content differs")
	}

	if _, err := ensureVerifiedCloudImage(context.Background(), srv.URL+"/", dir, keyring, nil); err != nil {
		t.Fatal(err)
	}
	if n := imageGets.Load(); n != 1 {
		t.Fatalf("a verified image on disk must be reused; downloaded %d times", n)
	}

	// A file on disk that no longer hashes to the signed digest is
	// downloaded again; a download that does not match is refused.
	if err := os.WriteFile(img.Path, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	serve = []byte("tampered in transit\n")
	if _, err := ensureVerifiedCloudImage(context.Background(), srv.URL+"/", dir, keyring, nil); err == nil {
		t.Fatal("an image that does not match the signed digest must be refused")
	}
}

func TestEnsureVerifiedCloudImage_RefusesUnsignedSums(t *testing.T) {
	sums := []byte(strings.Repeat("ab", 32) + " *" + cloudImageFile + "\n")
	keyring, _ := testSigner(t, sums)
	_, otherSig := testSigner(t, sums)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			_, _ = w.Write(sums)
		case "/SHA256SUMS.gpg":
			_, _ = w.Write(otherSig)
		default:
			t.Errorf("nothing but the sums may be fetched before they verify; got %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if _, err := ensureVerifiedCloudImage(context.Background(), srv.URL+"/", t.TempDir(), keyring, nil); err == nil {
		t.Fatal("sums signed by another key must be refused")
	}
}

func TestPruneCloudImages(t *testing.T) {
	dir := t.TempDir()
	base := strings.TrimSuffix(cloudImageFile, ".img")
	keep := filepath.Join(dir, "ubuntu-"+base+"-1111111111111111.img")
	old := filepath.Join(dir, "ubuntu-"+base+"-2222222222222222.img")
	other := filepath.Join(dir, "unrelated.img")
	for _, p := range []string{keep, old, other} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneCloudImages(dir, keep)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Fatalf("removed %v, want only %s", removed, old)
	}
	for _, p := range []string{keep, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must stay: %v", p, err)
		}
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cloudimage", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testSigner returns a fresh key's armored public keyring and its
// detached signature over data.
func testSigner(t *testing.T, data []byte) (armoredKeyring, sig []byte) {
	t.Helper()
	e, err := openpgp.NewEntity("y-cluster test", "", "test@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var pub bytes.Buffer
	w, err := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var s bytes.Buffer
	if err := openpgp.DetachSign(&s, e, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	return pub.Bytes(), s.Bytes()
}
