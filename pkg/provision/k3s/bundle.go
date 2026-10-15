package k3s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/shquote"
)

// A node bundle is everything y-cluster puts on a k3s server node
// before the platform goes on through the API: the k3s binary and its
// airgap images, the installer of the release, the server config and
// registries.yaml, as files in one directory with a manifest that says
// where each goes. Writing one needs neither the node nor root, and
// anything that can run commands as root on the node can apply it:
// ApplyBundle is y-cluster's own applier, which qemu provision uses.

// BundleManifestFile is the manifest's name in a bundle directory.
const BundleManifestFile = "manifest.yaml"

// BundleKind and BundleFormat identify the manifest. BundleFormat
// changes when an applier written against an earlier one would apply
// a bundle wrongly.
const (
	BundleKind   = "K3sNodeBundle"
	BundleFormat = 1
)

// RegistriesPath is where k3s reads its registries config. A bundle
// carries the file only when the spec has content for it; applying a
// bundle without one leaves the node's file, if any, alone.
const RegistriesPath = "/etc/rancher/k3s/registries.yaml"

// BundleSpec is what a bundle is made of.
type BundleSpec struct {
	// Version is the k3s release, e.g. v1.37.0+k3s1.
	Version string
	// Install is airgap (default) or script. A script bundle has no
	// binary and no images: the installer downloads k3s on the node.
	Install string
	// Arch is the node's architecture for an airgap bundle: amd64 or
	// arm64 (x86_64 and aarch64 are accepted as well).
	Arch string
	// Server is the content of ConfigPath.
	Server ServerSettings
	// Registries is the content of RegistriesPath; nil writes none.
	Registries []byte
	// PostInstall are shell commands to run as root after the
	// installer, in order.
	PostInstall []string
}

// BundleManifest is manifest.yaml. Paths in Files and Install.Script
// are relative to the bundle directory.
type BundleManifest struct {
	Kind   string `json:"kind"`
	Format int    `json:"format"`
	K3s    struct {
		Version string `json:"version"`
		Install string `json:"install"`
		// Arch is set for an airgap bundle, whose binary and images
		// run on that architecture only.
		Arch string `json:"arch,omitempty"`
	} `json:"k3s"`
	// Files are installed first, in order, as root-owned files with
	// the given mode. Parent directories that do not exist are created
	// with mode 0755.
	Files []BundleFile `json:"files"`
	// Install runs the installer as root with Env in its environment,
	// once Files are in place.
	Install BundleInstall `json:"install"`
	// PostInstall are shell commands run as root after the installer.
	PostInstall []string `json:"postInstall,omitempty"`
}

// BundleFile is one file to put on the node.
type BundleFile struct {
	Path   string `json:"path"`
	Dest   string `json:"dest"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

// BundleInstall is the installer step.
type BundleInstall struct {
	Script string            `json:"script"`
	SHA256 string            `json:"sha256"`
	Env    map[string]string `json:"env"`
}

// WriteBundle writes the bundle of spec into dir and returns its
// manifest. dir is created if missing; a directory that holds an
// earlier bundle has that bundle's files replaced, any other
// non-empty directory is refused. The manifest is written last, so a
// directory without one is not a bundle.
//
// The binary, images and installer come from the shared k3s cache
// (downloaded and checksum-verified there on first use) and are
// hard-linked into dir when it is on the same filesystem, copied
// otherwise. config.yaml and registries.yaml are written 0600:
// registries.yaml can carry registry credentials.
func WriteBundle(ctx context.Context, spec BundleSpec, dir string, logger *zap.Logger) (*BundleManifest, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if spec.Version == "" {
		return nil, errNoVersion
	}
	install := spec.Install
	if install == "" {
		install = "airgap"
	}
	m := &BundleManifest{Kind: BundleKind, Format: BundleFormat}
	m.K3s.Version = spec.Version
	m.K3s.Install = install
	if err := prepareBundleDir(dir); err != nil {
		return nil, err
	}

	switch install {
	case "airgap":
		art, arch, err := artifactsForArch(spec.Arch)
		if err != nil {
			return nil, err
		}
		m.K3s.Arch = arch
		binPath, imagesPath, err := cacheAirgap(ctx, spec.Version, art, logger)
		if err != nil {
			return nil, err
		}
		if err := linkOrCopy(binPath, filepath.Join(dir, "k3s"), 0o755); err != nil {
			return nil, err
		}
		if err := linkOrCopy(imagesPath, filepath.Join(dir, art.images), 0o644); err != nil {
			return nil, err
		}
		m.Files = append(m.Files,
			BundleFile{Path: "k3s", Dest: "/usr/local/bin/k3s", Mode: "0755"},
			BundleFile{Path: art.images, Dest: airgapImagesDir + "/" + art.images, Mode: "0644"},
		)
	case "script":
	default:
		return nil, fmt.Errorf("unknown k3s install %q (want airgap or script)", install)
	}

	config, err := spec.Server.Marshal()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), config, 0o600); err != nil {
		return nil, err
	}
	m.Files = append(m.Files, BundleFile{Path: "config.yaml", Dest: ConfigPath, Mode: "0600"})
	if spec.Registries != nil {
		if err := os.WriteFile(filepath.Join(dir, "registries.yaml"), spec.Registries, 0o600); err != nil {
			return nil, err
		}
		m.Files = append(m.Files, BundleFile{Path: "registries.yaml", Dest: RegistriesPath, Mode: "0600"})
	}

	scriptPath, err := cacheInstallScript(ctx, spec.Version, logger)
	if err != nil {
		return nil, err
	}
	if err := linkOrCopy(scriptPath, filepath.Join(dir, "install.sh"), 0o644); err != nil {
		return nil, err
	}
	m.Install = BundleInstall{Script: "install.sh", Env: installEnv(spec.Version, install == "airgap")}
	m.PostInstall = append([]string(nil), spec.PostInstall...)

	for i := range m.Files {
		if m.Files[i].SHA256, err = fileSHA256(filepath.Join(dir, m.Files[i].Path)); err != nil {
			return nil, err
		}
	}
	if m.Install.SHA256, err = fileSHA256(filepath.Join(dir, m.Install.Script)); err != nil {
		return nil, err
	}
	body, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, BundleManifestFile), body, 0o644); err != nil {
		return nil, err
	}
	logger.Info("k3s node bundle written", zap.String("dir", dir), zap.String("version", spec.Version),
		zap.String("install", install), zap.Int("files", len(m.Files)))
	return m, nil
}

// installEnv is the installer's environment. INSTALL_K3S_EXEC=server
// with no flags: every server option is in ConfigPath, so the systemd
// unit the installer writes runs plain `k3s server`.
func installEnv(version string, skipDownload bool) map[string]string {
	env := map[string]string{
		"INSTALL_K3S_VERSION": version,
		"INSTALL_K3S_EXEC":    "server",
	}
	if skipDownload {
		env["INSTALL_K3S_SKIP_DOWNLOAD"] = "true"
	}
	return env
}

// artifactsForArch maps a bundle architecture to release file names.
func artifactsForArch(arch string) (artifacts, string, error) {
	switch arch {
	case "", "amd64", "x86_64":
		a, err := artifactsFor("x86_64")
		return a, "amd64", err
	case "arm64", "aarch64":
		a, err := artifactsFor("aarch64")
		return a, "arm64", err
	default:
		return artifacts{}, "", fmt.Errorf("no k3s airgap artifacts for architecture %q (want amd64 or arm64)", arch)
	}
}

// unameArch maps `uname -m` output to a bundle architecture.
func unameArch(machine string) string {
	switch machine {
	case "x86_64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return machine
}

// prepareBundleDir makes dir ready for a bundle: created 0700 if
// missing, emptied of an earlier bundle's files, refused when it holds
// anything else.
func prepareBundleDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	prev, err := readManifest(dir)
	if err != nil {
		return fmt.Errorf("%s is not empty and holds no bundle (%v); give an empty or new directory", dir, err)
	}
	names := map[string]bool{BundleManifestFile: true, prev.Install.Script: true}
	for _, f := range prev.Files {
		names[f.Path] = true
	}
	for _, e := range entries {
		if !names[e.Name()] {
			return fmt.Errorf("%s holds %s, which is not part of the bundle in it; give an empty or new directory", dir, e.Name())
		}
	}
	// The manifest goes first: if a removal fails, what is left is not
	// mistaken for a complete bundle.
	if err := os.Remove(filepath.Join(dir, BundleManifestFile)); err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// linkOrCopy places src at dst, hard-linked when possible. A copy
// gets mode; a link keeps the cache file's mode, which the bundle does
// not rely on: the manifest carries the mode on the node.
func linkOrCopy(src, dst string, mode os.FileMode) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
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

func readManifest(dir string) (*BundleManifest, error) {
	body, err := os.ReadFile(filepath.Join(dir, BundleManifestFile))
	if err != nil {
		return nil, err
	}
	var m BundleManifest
	if err := yaml.UnmarshalStrict(body, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", BundleManifestFile, err)
	}
	if m.Kind != BundleKind || m.Format != BundleFormat {
		return nil, fmt.Errorf("%s is kind %q format %d; this y-cluster reads %s format %d", BundleManifestFile, m.Kind, m.Format, BundleKind, BundleFormat)
	}
	for _, p := range append(filePaths(m.Files), m.Install.Script) {
		if p == "" || p != path.Base(p) || p == "." || p == ".." || p == BundleManifestFile {
			return nil, fmt.Errorf("%s names %q; bundle paths are plain file names", BundleManifestFile, p)
		}
	}
	for _, f := range m.Files {
		if !path.IsAbs(f.Dest) || path.Clean(f.Dest) != f.Dest {
			return nil, fmt.Errorf("%s: dest %q of %s is not a clean absolute path", BundleManifestFile, f.Dest, f.Path)
		}
		if _, err := parseMode(f.Mode); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", BundleManifestFile, f.Path, err)
		}
	}
	return &m, nil
}

func filePaths(files []BundleFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func parseMode(s string) (os.FileMode, error) {
	if len(s) != 4 || s[0] != '0' {
		return 0, fmt.Errorf("mode %q is not four octal digits such as 0644", s)
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal", s)
	}
	return os.FileMode(n), nil
}

// ReadBundle reads the manifest of the bundle in dir and checks every
// file it lists against its sha256, so an applier never puts a
// truncated or altered file on a node.
func ReadBundle(dir string) (*BundleManifest, error) {
	m, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	check := func(name, want string) error {
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("bundle file %s has sha256 %s, the manifest says %s", name, got, want)
		}
		return nil
	}
	for _, f := range m.Files {
		if err := check(f.Path, f.SHA256); err != nil {
			return nil, err
		}
	}
	if err := check(m.Install.Script, m.Install.SHA256); err != nil {
		return nil, err
	}
	return m, nil
}

// EnvAssignments is Install.Env as shell-quoted NAME=value words in
// name order, for `env` or a command prefix.
func (i BundleInstall) EnvAssignments() []string {
	names := make([]string, 0, len(i.Env))
	for k := range i.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, k := range names {
		out = append(out, k+"="+shquote.Quote(i.Env[k]))
	}
	return out
}

// ApplyBundle applies the bundle in dir to a node through exec and
// copy, as a user with passwordless sudo: the files are copied to a
// private staging directory, installed as root with their modes, and
// checked against their sha256 where they landed; then the installer
// and the post-install commands run as root. It returns when the
// installer has started k3s, before the apiserver answers (WaitReady).
func ApplyBundle(ctx context.Context, exec NodeExec, copy NodeCopy, dir string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	m, err := ReadBundle(dir)
	if err != nil {
		return err
	}
	if m.K3s.Arch != "" {
		out, err := exec(ctx, "uname -m", nil)
		if err != nil {
			return fmt.Errorf("node architecture: %s: %w", out, err)
		}
		if got := unameArch(strings.TrimSpace(string(out))); got != m.K3s.Arch {
			return fmt.Errorf("the bundle is for %s and the node is %s", m.K3s.Arch, got)
		}
	}
	out, err := exec(ctx, "mktemp -d /tmp/y-cluster-k3s-bundle.XXXXXX", nil)
	if err != nil {
		return fmt.Errorf("staging directory on the node: %s: %w", out, err)
	}
	staging := strings.TrimSpace(string(out))
	if !strings.HasPrefix(staging, "/tmp/y-cluster-k3s-bundle.") || strings.ContainsAny(staging, " \n'\"") {
		return fmt.Errorf("unexpected staging directory %q", staging)
	}
	defer func() { _, _ = exec(context.WithoutCancel(ctx), "rm -rf "+shquote.Quote(staging), nil) }()

	for _, name := range append(filePaths(m.Files), m.Install.Script) {
		logger.Info("copying bundle file to the node", zap.String("file", name))
		if err := copy(ctx, filepath.Join(dir, name), staging+"/"+name); err != nil {
			return fmt.Errorf("copy %s to the node: %w", name, err)
		}
	}
	var sums bytes.Buffer
	for _, f := range m.Files {
		cmd := "sudo install -D -m " + f.Mode + " " + shquote.Quote(staging+"/"+f.Path) + " " + shquote.Quote(f.Dest)
		if out, err := exec(ctx, cmd, nil); err != nil {
			return fmt.Errorf("install %s: %s: %w", f.Dest, out, err)
		}
		fmt.Fprintf(&sums, "%s  %s\n", f.SHA256, f.Dest)
	}
	if out, err := exec(ctx, "sudo sha256sum -c --quiet -", bytes.NewReader(sums.Bytes())); err != nil {
		return fmt.Errorf("installed files differ from the bundle: %s: %w", out, err)
	}
	logger.Info("running the k3s installer", zap.String("version", m.K3s.Version), zap.String("install", m.K3s.Install))
	installCmd := "sudo env " + strings.Join(m.Install.EnvAssignments(), " ") + " sh " + shquote.Quote(staging+"/"+m.Install.Script)
	if out, err := exec(ctx, installCmd, nil); err != nil {
		return fmt.Errorf("k3s installer: %s: %w", out, err)
	}
	for _, c := range m.PostInstall {
		if out, err := exec(ctx, "sudo sh -c "+shquote.Quote(c), nil); err != nil {
			return fmt.Errorf("post-install %q: %s: %w", c, out, err)
		}
	}
	return nil
}
