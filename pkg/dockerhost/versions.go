package dockerhost

// The guest definition's pins. They change only through y-cluster
// releases: a release that moves one is how a security fix in dockerd,
// containerd or BuildKit reaches the guests, which pick it up when they
// are next recreated (at the latest after Config.MaxAge). Inside the
// guest the packages are held, so unattended-upgrades never moves them;
// it does keep patching the Ubuntu packages around them.
//
// Bumping: pick the newest patch release of each, check the Docker
// apt repository for the exact noble package versions
// (https://download.docker.com/linux/ubuntu/dists/noble/stable/binary-amd64/Packages)
// and take BuildKit's digest from the release's asset list, then run
// the e2e test (DOCKERHOST.md, Testing).
const (
	// DockerVersion is docker-ce and docker-ce-cli, from Docker's
	// apt repository.
	DockerVersion    = "29.8.2"
	dockerDebVersion = "5:" + DockerVersion + "-1~ubuntu.24.04~noble"

	// ContainerdVersion is containerd.io, dockerd's containerd, from
	// the same repository.
	ContainerdVersion    = "2.3.6"
	containerdDebVersion = ContainerdVersion + "-1~ubuntu.24.04~noble"

	// BuildKitVersion is buildkitd (and the buildctl next to it in
	// the guest), from the GitHub release, checked against
	// buildkitSHA256 before it is unpacked.
	BuildKitVersion = "v0.33.1"
	buildkitURL     = "https://github.com/moby/buildkit/releases/download/" + BuildKitVersion + "/buildkit-" + BuildKitVersion + ".linux-amd64.tar.gz"
	buildkitSHA256  = "4e044bcd62a0c0bbe6a8c94d73989de2bfe4c04dbc0f9d6021cf96b72cd1d965"

	// dockerAptKeyFingerprint is the key Docker signs its apt
	// repository with (guest/docker-apt.asc, from
	// https://download.docker.com/linux/ubuntu/gpg); apt checks the
	// repository's Release file against it in the guest.
	dockerAptKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
)
