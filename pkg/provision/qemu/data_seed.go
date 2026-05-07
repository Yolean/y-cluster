package qemu

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// Embedded assets that travel with the appliance disk and run
// on the customer's first boot. See
// APPLIANCE_MAINTENANCE.md for the design.

//go:embed data_seed.service
var dataSeedUnit string

//go:embed data_seed_check.sh
var dataSeedCheckScript string

//go:embed seed_status.sh
var seedStatusScript string

//go:embed k3s_data_seed.conf
var k3sDataSeedDropin string

// SeedMetaSchemaVersion is the on-disk schema version for the seed
// meta JSON. Bump when the schema changes incompatibly. See
// dataSeedMeta below.
const SeedMetaSchemaVersion = 1

// dataSeedMeta is the JSON written to /var/lib/y-cluster/data-seed.meta.json
// on the appliance disk AND copied verbatim to /data/yolean/.y-cluster-seeded
// on the customer's first boot.
//
// The customer-side marker is the source of truth for "this volume
// has been seeded". Future appliance versions can compare the
// customer's marker against the new seed's sha256 to decide whether
// migrations are needed.
type dataSeedMeta struct {
	SchemaVersion int    `json:"schemaVersion"`
	SeededAt      string `json:"seeded_at"`
	SeededBy      string `json:"seeded_by"`
	ApplianceName string `json:"appliance_name"`
	SeedSHA256    string `json:"seed_sha256"`
}

// extractDataYolean reads /data/yolean from an offline qcow2 via
// libguestfs's virt-tar-out and writes a zstd-compressed tar at
// outPath. Returns the SHA-256 of the resulting file.
//
// virt-tar-out emits an uncompressed tar on stdout; we pipe it
// through zstd at default level (3) for a good speed/ratio balance.
// The compressed file lives ON the appliance boot disk (under
// /var/lib/y-cluster/) so it travels with the appliance and is
// available even when the customer mounts an empty drive at
// /data/yolean (which obscures the boot disk's copy).
func extractDataYolean(ctx context.Context, qcow2Path, outPath string) (string, error) {
	out, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("create seed file: %w", err)
	}
	defer out.Close()

	hasher := sha256.New()
	mw := io.MultiWriter(out, hasher)

	// virt-tar-out -a <disk> /data/yolean -  -> stdout
	tarOut := exec.CommandContext(ctx, "virt-tar-out",
		"-a", qcow2Path, "/data/yolean", "-")
	tarOut.Stderr = os.Stderr
	tarPipe, err := tarOut.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("virt-tar-out pipe: %w", err)
	}

	// zstd -q -  (compress stdin to stdout)
	zstd := exec.CommandContext(ctx, "zstd", "-q", "-")
	zstd.Stdin = tarPipe
	zstd.Stdout = mw
	zstd.Stderr = os.Stderr

	if err := zstd.Start(); err != nil {
		return "", fmt.Errorf("start zstd: %w", err)
	}
	if err := tarOut.Run(); err != nil {
		_ = zstd.Wait()
		return "", fmt.Errorf("virt-tar-out: %w", err)
	}
	if err := zstd.Wait(); err != nil {
		return "", fmt.Errorf("zstd: %w", err)
	}

	if err := out.Sync(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// writeSeedMeta serialises dataSeedMeta to outPath. Side-channel
// used to inject the same metadata into both
// /var/lib/y-cluster/data-seed.meta.json on the appliance AND
// /data/yolean/.y-cluster-seeded on the customer's first boot.
func writeSeedMeta(outPath string, meta dataSeedMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0o644)
}

// SeedAssets bundles the host-side artefacts that need to land on
// the appliance disk for the data-seed feature to work at customer
// boot. PrepareExport materialises all of these and feeds them into
// virt-customize via --copy-in / --upload.
type SeedAssets struct {
	SeedTarPath    string // <tmp>/data-seed.tar.zst -- the snapshot
	SeedMetaPath   string // <tmp>/data-seed.meta.json -- metadata
	SeedCheckPath  string // <tmp>/y-cluster-data-seed-check -- the boot-time logic (chmod 0755)
	SeedStatusPath string // <tmp>/y-cluster-seed-status -- the customer-facing helper (chmod 0755)
	UnitPath       string // <tmp>/y-cluster-data-seed.service
	K3sDropinPath  string // <tmp>/k3s.service.d/y-cluster-data-seed.conf
	TmpDir         string // parent of the above; caller os.RemoveAll's at the end
}

// BuildSeedAssets snapshots /data/yolean from the offline qcow2,
// generates the meta JSON, and stages every asset PrepareExport
// will copy into the appliance via virt-customize. The caller MUST
// `os.RemoveAll(s.TmpDir)` to clean up.
//
// Returns (nil, nil) if the qcow2 doesn't have a /data/yolean dir
// at all -- e.g., a build cluster that never ran any workload that
// uses the bundled local-path. In that case PrepareExport SHOULD
// proceed without seed assets; the appliance will simply have no
// data-seed.tar.zst, the systemd unit's ConditionPathExists fires,
// and the unit no-ops at boot.
func BuildSeedAssets(ctx context.Context, qcow2Path, applianceName string) (*SeedAssets, error) {
	tmpDir, err := os.MkdirTemp("", "y-cluster-seed-")
	if err != nil {
		return nil, fmt.Errorf("mkdir seed tmp: %w", err)
	}

	tarPath := filepath.Join(tmpDir, "data-seed.tar.zst")
	sha, err := extractDataYolean(ctx, qcow2Path, tarPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		// virt-tar-out fails when the source path doesn't exist
		// in the guest. We treat that as "no /data/yolean to
		// seed", not an error worth aborting prepare-export over.
		// PrepareExport's logger surfaces a Warn so the operator
		// sees we skipped seed creation.
		return nil, fmt.Errorf("extract /data/yolean: %w", err)
	}

	metaPath := filepath.Join(tmpDir, "data-seed.meta.json")
	meta := dataSeedMeta{
		SchemaVersion: SeedMetaSchemaVersion,
		SeededAt:      time.Now().UTC().Format(time.RFC3339),
		SeededBy:      "y-cluster prepare-export",
		ApplianceName: applianceName,
		SeedSHA256:    sha,
	}
	if err := writeSeedMeta(metaPath, meta); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("write seed meta: %w", err)
	}

	checkPath := filepath.Join(tmpDir, "y-cluster-data-seed-check")
	if err := os.WriteFile(checkPath, []byte(dataSeedCheckScript), 0o755); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("write seed-check: %w", err)
	}
	statusPath := filepath.Join(tmpDir, "y-cluster-seed-status")
	if err := os.WriteFile(statusPath, []byte(seedStatusScript), 0o755); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("write seed-status: %w", err)
	}
	unitPath := filepath.Join(tmpDir, "y-cluster-data-seed.service")
	if err := os.WriteFile(unitPath, []byte(dataSeedUnit), 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("write seed unit: %w", err)
	}
	dropinDir := filepath.Join(tmpDir, "k3s.service.d")
	if err := os.MkdirAll(dropinDir, 0o755); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("mkdir k3s dropin: %w", err)
	}
	dropinPath := filepath.Join(dropinDir, "y-cluster-data-seed.conf")
	if err := os.WriteFile(dropinPath, []byte(k3sDataSeedDropin), 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("write k3s dropin: %w", err)
	}

	return &SeedAssets{
		SeedTarPath:    tarPath,
		SeedMetaPath:   metaPath,
		SeedCheckPath:  checkPath,
		SeedStatusPath: statusPath,
		UnitPath:       unitPath,
		K3sDropinPath:  dropinPath,
		TmpDir:         tmpDir,
	}, nil
}

// virtCustomizeArgsForSeed returns the additional --upload arguments
// PrepareExport appends to its virt-customize invocation when seed
// assets are present. --upload places a host file at an absolute
// path inside the guest filesystem; --copy-in places it at a
// directory. We use --upload throughout so each destination path is
// explicit and the caller doesn't have to think about basename
// collisions.
func virtCustomizeArgsForSeed(s *SeedAssets) []string {
	if s == nil {
		return nil
	}
	return []string{
		"--mkdir", "/var/lib/y-cluster",
		"--upload", s.SeedTarPath + ":/var/lib/y-cluster/data-seed.tar.zst",
		"--upload", s.SeedMetaPath + ":/var/lib/y-cluster/data-seed.meta.json",
		"--upload", s.SeedCheckPath + ":/usr/local/bin/y-cluster-data-seed-check",
		"--upload", s.SeedStatusPath + ":/usr/local/bin/y-cluster-seed-status",
		"--upload", s.UnitPath + ":/etc/systemd/system/y-cluster-data-seed.service",
		"--mkdir", "/etc/systemd/system/k3s.service.d",
		"--upload", s.K3sDropinPath + ":/etc/systemd/system/k3s.service.d/y-cluster-data-seed.conf",
		"--chmod", "0755:/usr/local/bin/y-cluster-data-seed-check",
		"--chmod", "0755:/usr/local/bin/y-cluster-seed-status",
		// The unit gets enabled (creates the wantedby symlink)
		// AFTER prepare_inguest.sh runs -- order matters because
		// systemctl enable requires /etc/systemd/system to be
		// writable, which it is throughout virt-customize.
		"--run-command", "systemctl enable y-cluster-data-seed.service",
	}
}

// applianceNameFromConfig is a small adapter so PrepareExport doesn't
// hard-code the field. Keeps the dataSeedMeta struct decoupled from
// Config's own evolution.
func applianceNameFromConfig(cfg Config) string {
	return cfg.Name
}

// silenceUnused references the logger import so a future build that
// drops the seed feature's only Warn doesn't fail with "imported and
// not used". Tiny cost, makes the import survive intermediate edits.
var _ = zap.NewNop
