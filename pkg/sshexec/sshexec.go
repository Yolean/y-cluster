// Package sshexec is the y-cluster SSH client used to talk to
// provisioner-managed machines (qemu VMs, hetzner servers) and to
// forward node commands from cluster.RunCtr / cluster.RunCrictl.
//
// It covers what would otherwise be three OpenSSH binaries: `ssh`
// (remote command), `scp` (file upload), `ssh-keygen` (ed25519
// key creation). The reason to do it in-process is error
// categorisation: x/crypto/ssh returns typed errors that the
// OpenSSH CLI flattens into "exit status 1":
//
//   - net.OpError("connection refused")    -> VM still booting
//   - *ssh.ServerAuthError                  -> key wrong, fatal
//   - *ssh.ExitError + ExitStatus()         -> remote command failed
//   - *ssh.ExitMissingError                 -> connection died mid-cmd
//
// All knownhost checks are intentionally disabled: the keys
// y-cluster targets are dev-cluster VMs we just brought up, and
// re-provisions rotate the host key. Hardening for production
// targets would belong elsewhere.
package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Target is the per-call connection coordinates.
type Target struct {
	Host    string // typically "127.0.0.1"
	Port    string // string for cobra-friendliness; parsed with net.JoinHostPort
	User    string // typically "ystack" for our cloud-init
	KeyPath string // private key file (no passphrase)
}

// handshakeTimeout bounds the ssh handshake when ctx has no earlier
// deadline. A variable so tests need not wait for it.
var handshakeTimeout = 10 * time.Second

// Dial opens an *ssh.Client, within ctx for both halves of that: the
// TCP connect and the ssh handshake.
//
// The handshake is the half that needs care. x/crypto/ssh takes no
// context, and ClientConfig.Timeout only covers the TCP connect of
// ssh.Dial, so NewClientConn waits for the server's banner for as
// long as it takes. A listener whose process is frozen or wedged (a
// paused qemu, a guest that hung during boot) completes the TCP
// connect from the kernel's backlog and then never sends a byte.
func Dial(ctx context.Context, t Target) (*ssh.Client, error) {
	cfg, err := clientConfig(t)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(t.Host, t.Port))
	if err != nil {
		return nil, err // net.OpError carries connection refused / timeout
	}

	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(t.Host, t.Port), cfg)
	stop()
	if err != nil {
		_ = conn.Close()
		return nil, ctxErr(ctx, err) // *ssh.ServerAuthError / *ssh.OpenChannelError typed
	}
	// The deadline was for the handshake; a session runs for as long
	// as its command does, bounded by ctx through closeOnDone.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = c.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// Exec runs cmd on the target and returns its stdout+stderr
// concatenated, like exec.Cmd.CombinedOutput. Errors are typed: *ssh.ExitError on
// non-zero remote exit, net.OpError on transport.
func Exec(ctx context.Context, t Target, cmd string, stdin io.Reader) ([]byte, error) {
	cli, err := Dial(ctx, t)
	if err != nil {
		return nil, err
	}
	defer closeOnDone(ctx, cli)()
	sess, err := cli.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	defer func() { _ = sess.Close() }()
	if stdin != nil {
		sess.Stdin = stdin
	}
	out, err := sess.CombinedOutput(cmd)
	return out, ctxErr(ctx, err)
}

// closeOnDone ties the connection to ctx for as long as the returned
// func has not run. The ssh session API takes no context, so a remote
// command that hangs (an installer waiting on the network, a guest
// that froze) would otherwise hold the caller past every timeout it
// set. Closing the client is what makes the blocked call return.
func closeOnDone(ctx context.Context, cli *ssh.Client) func() {
	stop := context.AfterFunc(ctx, func() { _ = cli.Close() })
	return func() {
		stop()
		_ = cli.Close()
	}
}

// ctxErr reports the context's error when that is why err happened:
// after closeOnDone fires, the ssh library only knows that its
// connection went away.
func ctxErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	return err
}

// ExecStream runs cmd with stdin/stdout/stderr wired to the
// caller's io.Reader / io.Writer. Used by cluster.RunCtr /
// cluster.RunCrictl so a `cat archive.tar | y-cluster ctr image
// import -` pipeline streams without buffering.
func ExecStream(ctx context.Context, t Target, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	cli, err := Dial(ctx, t)
	if err != nil {
		return err
	}
	defer closeOnDone(ctx, cli)()
	sess, err := cli.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer func() { _ = sess.Close() }()
	sess.Stdin = stdin
	sess.Stdout = stdout
	sess.Stderr = stderr
	return ctxErr(ctx, sess.Run(cmd))
}

// SCP uploads localPath to remotePath via SFTP. We use SFTP
// (RFC-defined, framed) rather than the legacy SCP wire
// protocol -- sftp.Client gives us typed errors per file
// operation and is what `pkg/sftp` is best at.
func SCP(ctx context.Context, t Target, localPath, remotePath string) error {
	cli, err := Dial(ctx, t)
	if err != nil {
		return err
	}
	defer closeOnDone(ctx, cli)()
	fc, err := sftp.NewClient(cli)
	if err != nil {
		return fmt.Errorf("sftp open: %w", err)
	}
	defer func() { _ = fc.Close() }()
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := fc.Create(remotePath)
	if err != nil {
		return fmt.Errorf("create %s: %w", remotePath, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return ctxErr(ctx, fmt.Errorf("write %s: %w", remotePath, err))
	}
	// Close is where the server acknowledges the last writes; a
	// failure here is a failed upload.
	if err := dst.Close(); err != nil {
		return ctxErr(ctx, fmt.Errorf("close %s: %w", remotePath, err))
	}
	return nil
}

// GenerateKey writes an ed25519 keypair to keyPath (private,
// PEM/OPENSSH format) and keyPath+".pub" (one-line OpenSSH
// authorized_keys format), as `ssh-keygen -t ed25519` would.
//
// The OpenSSH-format private key is what `ssh -i <key>` expects
// and what cloud-init's `ssh_authorized_keys` line consumes
// from `<key>.pub`.
func GenerateKey(keyPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ed25519: %w", err)
	}
	privBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return fmt.Errorf("marshal private: %w", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(privBlock), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	pubKey, err := ssh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("wrap public: %w", err)
	}
	authorized := ssh.MarshalAuthorizedKey(pubKey)
	if err := os.WriteFile(keyPath+".pub", authorized, 0o644); err != nil {
		return fmt.Errorf("write %s.pub: %w", keyPath, err)
	}
	return nil
}

func clientConfig(t Target) (*ssh.ClientConfig, error) {
	keyData, err := os.ReadFile(t.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", t.KeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", t.KeyPath, err)
	}
	return &ssh.ClientConfig{
		User:            t.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // dev cluster, see package doc
	}, nil
}
