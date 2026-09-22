package kubeconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// writeFileAtomic replaces path with data through a temp file and a
// rename, so a crash or a full disk mid-write leaves the operator's
// kubeconfig as it was instead of truncated.
//
// Two things a plain rename would change are kept as they are: a
// symlinked kubeconfig stays a symlink (the target is replaced), and
// a kubeconfig the operator made read-only stays refused. The second
// matters here: ystack keeps a 0444 ~/.kube/config precisely so that
// tools cannot write clusters into it, and a rename only needs write
// permission on the directory.
func writeFileAtomic(path string, data []byte) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	if fh, err := os.OpenFile(target, os.O_WRONLY, 0); err == nil {
		_ = fh.Close()
	} else if !os.IsNotExist(err) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone already after the rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// CreateTemp gives 0600 already; stated because the file holds
	// cluster credentials.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// lockTimeout bounds the wait for another y-cluster process that is
// in the middle of its own load-modify-save.
const lockTimeout = 30 * time.Second

// withFileLock runs fn while holding an exclusive lock for path, so
// two y-cluster processes that load, modify and save the same
// kubeconfig (parallel provisions) cannot lose each other's entries.
//
// flock(2), not a lock file's existence: it is released when the
// process dies, so there is no stale lock to clean up. The lock file
// has its own name because kubectl treats an existing <path>.lock as
// "locked" and would refuse to touch the kubeconfig if we left that
// one behind. This does not serialize against kubectl itself.
func withFileLock(path string, fn func() error) error {
	// A dotfile next to the kubeconfig. It is left in place: removing
	// a flock'd file lets a waiter lock the unlinked inode while a
	// newcomer locks a fresh one.
	lockPath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".y-cluster-lock")
	fh, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", lockPath, err)
	}
	defer func() { _ = fh.Close() }() // closing releases the lock

	deadline := time.Now().Add(lockTimeout)
	for {
		err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return fmt.Errorf("lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another y-cluster process held %s for more than %s", lockPath, lockTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fn()
}
