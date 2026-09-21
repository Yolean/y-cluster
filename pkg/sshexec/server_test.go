package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestServer runs an in-process ssh server that accepts any
// public key and understands three commands:
//
//	echo <text>   writes <text> to stdout, exits 0
//	fail          writes to stderr, exits 3
//	hang          never answers
func startTestServer(t *testing.T) Target {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTestConn(conn, cfg)
		}
	}()

	keyPath := filepath.Join(t.TempDir(), "id")
	if err := GenerateKey(keyPath); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	return Target{Host: host, Port: port, User: "ystack", KeyPath: keyPath}
}

func serveTestConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sconn.Close() }()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				// exec payload: uint32 length + command
				cmd := string(req.Payload[4:])
				status := uint32(0)
				switch {
				case strings.HasPrefix(cmd, "echo "):
					_, _ = ch.Write([]byte(strings.TrimPrefix(cmd, "echo ") + "\n"))
				case cmd == "fail":
					_, _ = ch.Stderr().Write([]byte("it broke\n"))
					status = 3
				case cmd == "hang":
					continue // no exit-status, no close: the client waits
				}
				payload := make([]byte, 4)
				binary.BigEndian.PutUint32(payload, status)
				_, _ = ch.SendRequest("exit-status", false, payload)
				_ = ch.Close()
			}
		}()
	}
}

func TestExec_OutputAndExitStatus(t *testing.T) {
	target := startTestServer(t)

	out, err := Exec(context.Background(), target, "echo hello", nil)
	if err != nil || strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("echo: out=%q err=%v", out, err)
	}

	out, err = Exec(context.Background(), target, "fail", nil)
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
		t.Fatalf("want *ssh.ExitError with status 3, got %v", err)
	}
	if !strings.Contains(string(out), "it broke") {
		t.Errorf("stderr should be part of the combined output, got %q", out)
	}
}

// The ssh session API takes no context. A remote command that never
// returns used to hold Exec, and everything above it (k3s install,
// the ssh wait loop), past every timeout the caller had set.
func TestExec_HangingCommandHonoursContext(t *testing.T) {
	target := startTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Exec(ctx, target, "hang", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Exec returned after %s", elapsed)
	}
}

func TestExecStream_HangingCommandHonoursContext(t *testing.T) {
	target := startTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := ExecStream(ctx, target, "hang", nil, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}
