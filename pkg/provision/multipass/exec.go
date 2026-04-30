package multipass

import (
	"context"
	"io"

	"github.com/Yolean/y-cluster/pkg/multipassexec"
)

// runMultipass / multipassExec / multipassTransfer / multipassInfo /
// multipassStop / multipassDelete / multipassVersion are thin
// re-exports of the pkg/multipassexec API used by the rest of this
// package. The shared package is the source of truth; this file
// keeps the call sites short and provider-local.

func runMultipass(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return multipassexec.Run(ctx, stdin, args...)
}

func multipassExec(ctx context.Context, name, command string, stdin io.Reader) ([]byte, error) {
	return multipassexec.Exec(ctx, name, command, stdin)
}

func multipassTransfer(ctx context.Context, name, localPath, remotePath string) error {
	return multipassexec.Transfer(ctx, name, localPath, remotePath)
}

type vmInfo = multipassexec.VMInfo

func multipassInfo(ctx context.Context, name string) (*vmInfo, error) {
	return multipassexec.Info(ctx, name)
}

func firstIPv4(info *vmInfo) string { return multipassexec.FirstIPv4(info) }

func multipassStop(ctx context.Context, name string) error {
	return multipassexec.Stop(ctx, name)
}

func multipassDelete(ctx context.Context, name string, purge bool) error {
	return multipassexec.Delete(ctx, name, purge)
}

func multipassVersion(ctx context.Context) error {
	return multipassexec.Version(ctx)
}

// errVMNotFound aliases multipassexec.ErrNotFound so callers in
// this package don't need to import multipassexec just for the
// sentinel.
var errVMNotFound = multipassexec.ErrNotFound
