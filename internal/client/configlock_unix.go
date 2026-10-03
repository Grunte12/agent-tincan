//go:build unix

package client

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// configLockWait bounds how long a client waits for another process to
// release the config lock, so a stalled writer cannot hold up a relay move;
// a package var so tests can shorten it.
var configLockWait = 2 * time.Second

// lockConfig takes an advisory lock on the config file at path (through
// path.lock beside it) and returns the function that releases it. Clients
// in several processes that read, change and save the same file hold it
// so one does not undo another's write. It gives up after configLockWait.
func lockConfig(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(configLockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errors.New("another process holds the config lock")
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
