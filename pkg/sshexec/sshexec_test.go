package sshexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestGenerateKey_RoundTrip generates a key and round-trips it
// through ssh.ParsePrivateKey + ssh.NewPublicKey to confirm both
// halves are byte-for-byte what the OpenSSH client / cloud-init
// would consume.
func TestGenerateKey_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}

	priv, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("parse private: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("unexpected key type %q", signer.PublicKey().Type())
	}

	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pub), "ssh-ed25519 ") {
		t.Fatalf(".pub should start with ssh-ed25519, got %q", pub)
	}

	parsedPub, _, _, _, err := ssh.ParseAuthorizedKey(pub)
	if err != nil {
		t.Fatalf("parse authorized key: %v", err)
	}
	// Marshal both sides back to wire bytes and compare — the
	// signer's public key should match the .pub file's parsed key.
	if string(parsedPub.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Fatal("private/public key pair does not match on disk")
	}
}

func TestGenerateKey_OverwriteSafe(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(keyPath)
	if err := GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(keyPath)
	if string(first) == string(second) {
		t.Fatal("regenerating should produce a different key")
	}
}

func TestGenerateKey_FilePerms(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// SSH refuses to use a private key with permissions wider
	// than 0600, so we must write it that way.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("private key perm %o, want 0600", perm)
	}
	pubInfo, err := os.Stat(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if perm := pubInfo.Mode().Perm(); perm != 0o644 {
		t.Fatalf("public key perm %o, want 0644", perm)
	}
}
