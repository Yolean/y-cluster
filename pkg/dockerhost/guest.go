package dockerhost

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"

	"github.com/Yolean/y-cluster/pkg/provision/qemu"
)

// The guest definition: what cloud-init writes and runs on the first
// boot of a dockerhost guest, on top of the qemu provider's own user
// (ystack, with the provision's ssh key) and network configuration.
// Everything the guest needs comes from here; nothing is mounted or
// copied from the host besides these files.

//go:embed guest
var guestFS embed.FS

const (
	// DockerPort is dockerd's TCP port in the guest: TLS with client
	// verification, Docker's convention for it.
	DockerPort = "2376"
	// BuildkitPort is buildkitd's TCP port in the guest, the one
	// ystack's in-cluster buildkitd uses.
	BuildkitPort = "8547"

	// guestTLSDir holds the CA certificate and the server's
	// certificate and key in the guest, for both daemons.
	guestTLSDir = "/etc/y-cluster-dockerhost/tls"
	// guestIdleCommand is the reaper's script; provision runs its
	// lease and status verbs over ssh.
	guestIdleCommand  = "/usr/local/sbin/y-cluster-dockerhost-idle"
	guestSetupCommand = "/usr/local/sbin/y-cluster-dockerhost-setup"
)

type guestTemplateData struct {
	TLSDir       string
	DockerPort   string
	BuildkitPort string
}

// guestSpec is the qemu GuestSpec of a dockerhost guest: the server's
// TLS material (never the CA's key, which no one keeps, or the
// client's), the daemons' configuration, the pins, the idle reaper and
// the setup script cloud-init runs once.
func guestSpec(cloudImage string, tls tlsMaterial, limits reaperLimits) (qemu.GuestSpec, error) {
	data := guestTemplateData{TLSDir: guestTLSDir, DockerPort: DockerPort, BuildkitPort: BuildkitPort}
	file := func(path, perm, content string) qemu.GuestFile {
		return qemu.GuestFile{Path: path, Permissions: perm, Content: content}
	}
	var files []qemu.GuestFile
	add := func(path, perm, src string) error {
		content, err := renderGuestFile(src, data)
		if err != nil {
			return err
		}
		files = append(files, file(path, perm, content))
		return nil
	}
	files = append(files,
		file(guestTLSDir+"/ca.pem", "0644", string(tls.CACert)),
		file(guestTLSDir+"/server-cert.pem", "0644", string(tls.ServerCert)),
		file(guestTLSDir+"/server-key.pem", "0600", string(tls.ServerKey)),
		file("/etc/y-cluster-dockerhost/pins.env", "0644", pinsEnv()),
		file("/etc/y-cluster-dockerhost/reaper.conf", "0644", limits.conf()),
	)
	for _, f := range []struct{ path, perm, src string }{
		{"/etc/apt/keyrings/docker.asc", "0644", "guest/docker-apt.asc"},
		{"/etc/docker/daemon.json", "0644", "guest/daemon.json.tmpl"},
		{"/etc/systemd/system/docker.service.d/y-cluster-dockerhost.conf", "0644", "guest/docker-override.conf.tmpl"},
		{"/etc/buildkit/buildkitd.toml", "0644", "guest/buildkitd.toml.tmpl"},
		{"/etc/systemd/system/buildkit.service", "0644", "guest/buildkit.service"},
		{"/etc/systemd/system/y-cluster-dockerhost-idle.service", "0644", "guest/idle.service"},
		{"/etc/systemd/system/y-cluster-dockerhost-idle.timer", "0644", "guest/idle.timer"},
		{guestIdleCommand, "0755", "guest/idle.sh"},
		{guestSetupCommand, "0755", "guest/setup.sh"},
	} {
		if err := add(f.path, f.perm, f.src); err != nil {
			return qemu.GuestSpec{}, err
		}
	}
	return qemu.GuestSpec{
		CloudImage: cloudImage,
		Files:      files,
		RunCmd:     []string{guestSetupCommand},
	}, nil
}

// renderGuestFile reads src from the embedded guest directory and, for
// a .tmpl file, executes it with data.
func renderGuestFile(src string, data guestTemplateData) (string, error) {
	raw, err := guestFS.ReadFile(src)
	if err != nil {
		return "", err
	}
	if len(src) < 5 || src[len(src)-5:] != ".tmpl" {
		return string(raw), nil
	}
	t, err := template.New(src).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", src, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("render %s: %w", src, err)
	}
	return b.String(), nil
}

// pinsEnv is sourced by the setup script.
func pinsEnv() string {
	return fmt.Sprintf("DOCKER_DEB_VERSION='%s'\nCONTAINERD_DEB_VERSION='%s'\nBUILDKIT_VERSION='%s'\nBUILDKIT_URL='%s'\nBUILDKIT_SHA256='%s'\n",
		dockerDebVersion, containerdDebVersion, BuildKitVersion, buildkitURL, buildkitSHA256)
}

// reaperLimits are the idle reaper's limits in seconds, 0 for off.
type reaperLimits struct {
	IdleTimeout int64
	MaxAge      int64
}

func (l reaperLimits) conf() string {
	return fmt.Sprintf("IDLE_TIMEOUT=%d\nMAX_AGE=%d\n", l.IdleTimeout, l.MaxAge)
}

// leaseCommand is what provision runs in the guest over ssh.
func (l reaperLimits) leaseCommand() string {
	return fmt.Sprintf("sudo %s lease %d %d", guestIdleCommand, l.IdleTimeout, l.MaxAge)
}
