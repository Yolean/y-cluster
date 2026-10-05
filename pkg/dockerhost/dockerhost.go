// Package dockerhost runs dockerd and buildkitd in a KVM guest, made
// by the qemu provider, and tells clients how to reach them: builds and
// Testcontainers suites run on the host as clients of a daemon that is
// root only inside the guest. One guest per machine, shared by every
// session of the user, reached over TLS with client certificates on an
// address of its own on a tap device that root prepared once.
// DOCKERHOST.md in the repository root is the design and the operator's
// guide.
package dockerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/Yolean/y-cluster/pkg/inventory"
	"github.com/Yolean/y-cluster/pkg/provision/qemu"
	"github.com/Yolean/y-cluster/pkg/shquote"
)

// Options are Provision's inputs.
type Options struct {
	// Dir is the state directory, DefaultDir.
	Dir string
	// Config is defaulted and valid (LoadConfig, or ApplyDefaults and
	// Validate).
	Config Config
	// ImageDir holds the verified cloud images. Empty is Dir/images,
	// where provision keeps only the image the current guest is backed
	// by; a directory given here is shared and never pruned.
	ImageDir string
}

// Outcome is what Provision did.
type Outcome string

const (
	// Reused: a running, healthy guest.
	Reused Outcome = "reused"
	// Restarted: a stopped guest (powered off by its idle reaper)
	// booted again from its disk, image and build caches intact.
	Restarted Outcome = "restarted"
	// Created: a new guest from the newest signed cloud image.
	Created Outcome = "created"
)

// Result is Provision's answer.
type Result struct {
	State   State
	Outcome Outcome
	// Replaced says why an existing guest was torn down first.
	Replaced string
}

const (
	// hardAgeGrace: past its maximum age a guest that is in use right
	// now is left alone, for this long at most.
	hardAgeGrace = 24 * time.Hour
	// cloudInitTimeout bounds the first boot: package installs and two
	// downloads.
	cloudInitTimeout = 15 * time.Minute
	// readyTimeout bounds the wait for both daemons after a boot.
	readyTimeout = 3 * time.Minute
	// inventoryKey names the host inventory record. Not a kubeconfig
	// context, but unlikely to be one.
	inventoryKey = "y-cluster-dockerhost"
)

// Test seams.
var (
	nowFunc      = time.Now
	guestRunning = func(vmDir string) (int, bool) { return qemu.GuestPID(vmDir, guestName) }
	guestExec    = func(ctx context.Context, vmDir, command string) ([]byte, error) {
		return qemu.GuestExec(ctx, vmDir, guestName, command)
	}
)

// Provision makes sure the machine's dockerhost guest runs and answers,
// and returns its state. It is idempotent and cheap when the guest is
// healthy, so sessions call it before every build or test run; each
// call also renews the guest's lease against the idle reaper.
//
// An existing guest is reused when it runs and answers, booted again
// when its idle reaper powered it off, and replaced by a new one from
// the newest signed cloud image when it is older than the maximum age
// (once nobody uses it, and in any case a day later), when this
// y-cluster pins other daemon versions (once nobody uses it), when the
// configuration changed, or when it does not answer.
func Provision(ctx context.Context, opts Options, logger *zap.Logger) (Result, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if err := opts.Config.Validate(); err != nil {
		return Result{}, err
	}
	p := paths(opts.Dir)
	var res Result
	err := withLock(p, func() error {
		var err error
		res, err = provisionLocked(ctx, p, opts, logger)
		return err
	})
	return res, err
}

func provisionLocked(ctx context.Context, p paths, opts Options, logger *zap.Logger) (Result, error) {
	limits, err := opts.Config.limits()
	if err != nil {
		return Result{}, err
	}
	replaced := ""
	st, err := loadState(p)
	switch {
	case errors.Is(err, errNoState):
	case err != nil:
		return Result{}, err
	default:
		res, reason := useExisting(ctx, p, st, opts.Config, limits, logger)
		if res != nil {
			return *res, nil
		}
		logger.Info("replacing the dockerhost guest", zap.String("reason", reason))
		if err := teardownLocked(p, logger); err != nil {
			return Result{}, err
		}
		replaced = reason
	}
	res, err := create(ctx, p, opts, limits, logger)
	res.Replaced = replaced
	return res, err
}

// action is decideExisting's verdict on a guest that has state.
type action string

const (
	actReuse   action = "reuse"
	actRestart action = "restart"
	actReplace action = "replace"
)

// decideExisting is the whole of provision's judgement on an existing
// guest, before anything is asked of the guest itself; busy is only
// called when the answer depends on whether someone uses the guest.
func decideExisting(st State, cfg Config, running bool, now time.Time, busy func() bool) (action, string) {
	if st.ReadyAt.IsZero() {
		return actReplace, "its first boot did not finish"
	}
	if d := drift(st, cfg); d != "" {
		return actReplace, "the configuration changed: " + d
	}
	maxAge, _ := cfg.maxAge()
	age := now.Sub(st.CreatedAt)
	if age >= maxAge+hardAgeGrace {
		return actReplace, fmt.Sprintf("it is %s old, a day past the maximum age of %s", age.Round(time.Minute), maxAge)
	}
	soft := ""
	switch {
	case age >= maxAge:
		soft = fmt.Sprintf("it is %s old, past the maximum age of %s", age.Round(time.Minute), maxAge)
	case st.Pins != currentPins():
		soft = fmt.Sprintf("this y-cluster pins %s, the guest runs %s", pinsString(currentPins()), pinsString(st.Pins))
	}
	if !running {
		if soft != "" {
			return actReplace, soft
		}
		return actRestart, "it is stopped"
	}
	if soft != "" && !busy() {
		return actReplace, soft + ", and nobody uses it"
	}
	return actReuse, soft
}

func pinsString(p Pins) string {
	return fmt.Sprintf("docker %s, containerd %s, buildkit %s", p.Docker, p.Containerd, p.BuildKit)
}

// drift lists what of the configuration the guest was made with
// differs from cfg, beyond the reaper's limits (a lease updates those).
func drift(st State, cfg Config) string {
	var d []string
	if (st.TestForwards == nil) != (cfg.TestForwards == nil) ||
		(st.TestForwards != nil && *st.TestForwards != *cfg.TestForwards) {
		d = append(d, "network mode")
	}
	if cfg.TestForwards == nil {
		a, b := st.Network, cfg.Network
		if a.Ifname != b.Ifname || a.GuestAddress != b.GuestAddress || a.Gateway != b.Gateway || strings.Join(a.DNS, ",") != strings.Join(b.DNS, ",") {
			d = append(d, "network")
		}
	}
	if st.MemoryMB != cfg.MemoryMB || st.CPUs != cfg.CPUs || st.DiskSize != cfg.DiskSize {
		d = append(d, "size")
	}
	return strings.Join(d, ", ")
}

// useExisting reuses or restarts the guest st describes. A nil result
// means it has to be replaced, for the reason given.
func useExisting(ctx context.Context, p paths, st State, cfg Config, limits reaperLimits, logger *zap.Logger) (*Result, string) {
	_, running := guestRunning(p.vm())
	busy := func() bool { return guestBusy(ctx, p, logger) }
	act, reason := decideExisting(st, cfg, running, nowFunc(), busy)
	switch act {
	case actReplace:
		return nil, reason
	case actRestart:
		logger.Info("booting the stopped dockerhost guest from its disk")
		if _, err := qemu.StartGuest(ctx, p.vm(), guestName, logger); err != nil {
			return nil, "it did not boot again: " + err.Error()
		}
		if err := waitHealthy(ctx, st, p.client(), logger); err != nil {
			return nil, "it did not answer after booting again: " + err.Error()
		}
		st.BootedAt = nowFunc()
	default:
		if reason != "" {
			logger.Warn("dockerhost guest kept while in use; it is replaced once idle", zap.String("why", reason))
		}
		if h := checkHealth(ctx, st, p.client()); !h.OK() {
			return nil, "it does not answer: " + h.Err().Error()
		}
	}
	renewLease(ctx, p, limits, logger)
	st.IdleTimeout, st.MaxAge = cfg.IdleTimeout, cfg.MaxAge
	if err := saveState(p, st); err != nil {
		logger.Warn("could not save dockerhost state", zap.Error(err))
	}
	recordInventory(p, st, logger)
	out := Reused
	if act == actRestart {
		out = Restarted
	}
	return &Result{State: st, Outcome: out}, ""
}

// create makes a new guest. On failure the half-made guest is torn
// down, its console log kept as last-failure-console.log.
func create(ctx context.Context, p paths, opts Options, limits reaperLimits, logger *zap.Logger) (Result, error) {
	cfg := opts.Config
	qcfg, err := cfg.qemuConfig(p.vm())
	if err != nil {
		return Result{}, err
	}
	if err := qemu.CheckPrerequisites(); err != nil {
		return Result{}, err
	}
	imageDir, prune := opts.ImageDir, false
	if imageDir == "" {
		imageDir, prune = p.images(), true
	}
	img, err := qemu.EnsureVerifiedCloudImage(ctx, imageDir, logger)
	if err != nil {
		return Result{}, err
	}
	ep := cfg.endpoint()
	now := nowFunc()
	material, err := issueTLS(ep.Address, now)
	if err != nil {
		return Result{}, err
	}
	spec, err := guestSpec(img.Path, material, limits)
	if err != nil {
		return Result{}, err
	}
	st := State{
		TestForwards: cfg.TestForwards,
		Address:      ep.Address.String(),
		DockerPort:   ep.DockerPort,
		BuildkitPort: ep.BuildkitPort,
		MemoryMB:     cfg.MemoryMB,
		CPUs:         cfg.CPUs,
		DiskSize:     cfg.DiskSize,
		IdleTimeout:  cfg.IdleTimeout,
		MaxAge:       cfg.MaxAge,
		CreatedAt:    now,
		CertNotAfter: material.NotAfter,
		CloudImage:   ImageState{SHA256: img.SHA256, URL: img.URL, SignedBy: img.SignedBy, SignedAt: img.SignedAt},
		Pins:         currentPins(),
	}
	if cfg.TestForwards == nil {
		st.Network = cfg.Network
	}
	// Recorded before anything is started, so a teardown finds a guest
	// whose provision was interrupted.
	if err := saveState(p, st); err != nil {
		return Result{}, err
	}
	fail := func(err error) (Result, error) {
		keepConsoleLog(p, logger)
		logger.Warn("dockerhost provision failed; removing the half-made guest", zap.Error(err))
		if terr := teardownLocked(p, logger); terr != nil {
			logger.Warn("teardown after the failed provision failed too", zap.Error(terr))
		}
		return Result{}, err
	}
	if err := writeClientDir(p.client(), material); err != nil {
		return fail(err)
	}
	logger.Info("creating the dockerhost guest",
		zap.String("address", st.Address),
		zap.String("image", img.SHA256[:16]),
		zap.String("docker", DockerVersion), zap.String("containerd", ContainerdVersion), zap.String("buildkit", BuildKitVersion))
	if _, err := qemu.ProvisionGuest(ctx, qcfg, spec, logger); err != nil {
		return fail(err)
	}
	if err := waitCloudInit(ctx, p, logger); err != nil {
		return fail(err)
	}
	if err := qemu.RemoveGuestUserData(p.vm(), guestName); err != nil {
		logger.Warn("could not remove the guest's user-data", zap.Error(err))
	}
	if err := waitHealthy(ctx, st, p.client(), logger); err != nil {
		return fail(err)
	}
	st.ReadyAt = nowFunc()
	st.BootedAt = st.ReadyAt
	if err := saveState(p, st); err != nil {
		return fail(err)
	}
	recordInventory(p, st, logger)
	if prune {
		if removed, err := qemu.PruneCloudImages(imageDir, img.Path); err != nil {
			logger.Warn("could not prune old cloud images", zap.Error(err))
		} else if len(removed) > 0 {
			logger.Info("pruned old cloud images", zap.Strings("removed", removed))
		}
	}
	return Result{State: st, Outcome: Created}, nil
}

// waitCloudInit polls cloud-init in the guest until the first boot is
// over. An error carries the end of the setup output.
func waitCloudInit(ctx context.Context, p paths, logger *zap.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, cloudInitTimeout)
	defer cancel()
	logger.Info("waiting for the guest's first boot (cloud-init: dockerd, buildkitd)", zap.Duration("timeout", cloudInitTimeout))
	start := time.Now()
	last := ""
	for {
		out, err := guestExec(ctx, p.vm(), "cloud-init status")
		if err == nil || len(out) > 0 {
			last = strings.TrimSpace(string(out))
		}
		switch {
		case strings.Contains(last, "status: done"):
			logger.Info("first boot done", zap.Duration("took", time.Since(start).Round(time.Second)))
			return nil
		case strings.Contains(last, "status: error"):
			tail, _ := guestExec(context.Background(), p.vm(), "sudo tail -n 60 /var/log/cloud-init-output.log")
			return fmt.Errorf("the guest's first boot failed (cloud-init %s); end of its output:\n%s", last, tail)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the guest's first boot did not finish within %s (cloud-init %q)", cloudInitTimeout, last)
		case <-time.After(5 * time.Second):
		}
	}
}

// waitHealthy polls both daemons until they answer.
func waitHealthy(ctx context.Context, st State, clientDir string, logger *zap.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for {
		h := checkHealth(ctx, st, clientDir)
		if h.OK() {
			logger.Info("dockerd and buildkitd answer over TLS", zap.String("dockerd", h.Docker))
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no answer within %s: %w", readyTimeout, h.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

// renewLease tells the guest's idle reaper that someone wants the
// guest, and passes it the current limits. Best-effort.
func renewLease(ctx context.Context, p paths, limits reaperLimits, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := guestExec(ctx, p.vm(), limits.leaseCommand()); err != nil {
		logger.Warn("could not renew the guest's lease; its idle reaper counts from the last renewal", zap.Error(err), zap.ByteString("output", out))
	}
}

// guestBusy asks the guest's reaper whether anyone uses it. Unknown
// reads as idle: a guest that cannot answer ssh is no one's.
func guestBusy(ctx context.Context, p paths, logger *zap.Logger) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := guestExec(ctx, p.vm(), "sudo "+guestIdleCommand+" status")
	if err != nil {
		logger.Warn("could not ask the guest whether it is in use", zap.Error(err))
		return false
	}
	return strings.TrimSpace(string(out)) == "busy"
}

func keepConsoleLog(p paths, logger *zap.Logger) {
	src := filepath.Join(p.vm(), guestName+"-console.log")
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	dst := filepath.Join(p.dir(), "last-failure-console.log")
	if err := os.WriteFile(dst, data, 0o600); err == nil {
		logger.Info("kept the failed guest's console log", zap.String("path", dst))
	}
}

// Teardown stops and removes the guest, its disk, its certificates and
// its state. The verified cloud images stay for the next provision.
// Idempotent.
func Teardown(dir string, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	p := paths(dir)
	if _, err := os.Stat(p.dir()); os.IsNotExist(err) {
		logger.Info("no dockerhost on this machine", zap.String("dir", p.dir()))
		return nil
	}
	return withLock(p, func() error { return teardownLocked(p, logger) })
}

func teardownLocked(p paths, logger *zap.Logger) error {
	// qemu's teardown: a graceful poweroff over ssh, signals when that
	// fails, then every file of the VM.
	if err := qemu.TeardownConfig(qemu.Config{Name: guestName, CacheDir: p.vm()}, false, logger); err != nil {
		return err
	}
	_ = os.Remove(p.vm()) // only when empty
	if err := os.RemoveAll(p.client()); err != nil {
		return err
	}
	if err := os.Remove(p.state()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := inventory.Remove(inventoryKey); err != nil {
		logger.Warn("inventory record removal failed", zap.Error(err))
	}
	return nil
}

func recordInventory(p paths, st State, logger *zap.Logger) {
	rec := inventory.Record{
		Context:   inventoryKey,
		Name:      guestName,
		Provider:  "dockerhost",
		ConfigDir: p.dir(),
		Teardown:  teardownCommand(p),
	}
	if f := st.TestForwards; f != nil {
		rec.HostPorts = []string{f.SSH, f.Docker, f.Buildkit}
	}
	if err := inventory.Save(rec); err != nil {
		logger.Warn("inventory record write failed", zap.Error(err))
	}
}

func teardownCommand(p paths) string {
	if home, err := os.UserHomeDir(); err == nil && p.dir() == filepath.Join(home, ".cache", "y-cluster-dockerhost") {
		return "y-cluster dockerhost teardown"
	}
	return DirEnv + "=" + shquote.Quote(p.dir()) + " y-cluster dockerhost teardown"
}

// EnvVars are the variables of the client contract, in the order Env
// prints them.
var EnvVars = []string{"DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "BUILDKIT_HOST", "BUILDKIT_TLS_DIR"}

// Env returns the shell lines `eval "$(y-cluster dockerhost env)"`
// runs. With a guest that runs, the contract:
//
//	export DOCKER_HOST=tcp://<guest address>:2376
//	export DOCKER_TLS_VERIFY=1
//	export DOCKER_CERT_PATH=<client dir>
//	export BUILDKIT_HOST=tcp://<guest address>:8547
//	export BUILDKIT_TLS_DIR=<client dir>
//
// Without one, nothing: a machine with plain Docker, or with a
// DOCKER_HOST someone set on purpose, keeps what it has. The one
// exception is a shell that still carries this dockerhost's own
// variables (DOCKER_CERT_PATH or BUILDKIT_TLS_DIR is this machine's
// client directory): it gets an unset line for all five, so it falls
// back to the local socket instead of dialing a guest that is gone.
// getenv is the calling shell's environment.
func Env(dir string, getenv func(string) string) (string, error) {
	p := paths(dir)
	st, err := loadState(p)
	if err != nil && !errors.Is(err, errNoState) {
		return "", err
	}
	if err == nil && !st.ReadyAt.IsZero() {
		if _, running := guestRunning(p.vm()); running {
			return renderEnv(st, p.client()), nil
		}
	}
	if c := p.client(); getenv("DOCKER_CERT_PATH") == c || getenv("BUILDKIT_TLS_DIR") == c {
		return "unset " + strings.Join(EnvVars, " ") + "\n", nil
	}
	return "", nil
}

func renderEnv(st State, clientDir string) string {
	values := map[string]string{
		"DOCKER_HOST":       st.DockerHostURL(),
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_CERT_PATH":  clientDir,
		"BUILDKIT_HOST":     st.BuildkitHostURL(),
		"BUILDKIT_TLS_DIR":  clientDir,
	}
	var b strings.Builder
	for _, k := range EnvVars {
		fmt.Fprintf(&b, "export %s=%s\n", k, shquote.Quote(values[k]))
	}
	return b.String()
}

// Status is what `y-cluster dockerhost status` reports.
type Status struct {
	Dir       string
	State     *State
	PID       int
	Running   bool
	Health    *Health
	Activity  string // busy, idle, or why it is unknown
	SSH       string
	ClientDir string
}

// Healthy reports a guest that runs and answers.
func (s Status) Healthy() bool { return s.Running && s.Health != nil && s.Health.OK() }

// GetStatus reads the state and, when the guest runs, asks it.
func GetStatus(ctx context.Context, dir string) (Status, error) {
	p := paths(dir)
	s := Status{Dir: p.dir(), ClientDir: p.client()}
	st, err := loadState(p)
	if errors.Is(err, errNoState) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.State = &st
	s.PID, s.Running = guestRunning(p.vm())
	if cfg, err := qemu.GuestConfig(p.vm(), guestName); err == nil {
		s.SSH = cfg.SSHCommand()
	}
	if !s.Running {
		return s, nil
	}
	h := checkHealth(ctx, st, p.client())
	s.Health = &h
	ectx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if out, err := guestExec(ectx, p.vm(), "sudo "+guestIdleCommand+" status"); err == nil {
		s.Activity = strings.TrimSpace(string(out))
	} else {
		s.Activity = "unknown: " + err.Error()
	}
	return s, nil
}
