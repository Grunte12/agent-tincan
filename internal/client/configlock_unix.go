//go:build unix

package client

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockConfig takes an advisory lock on the config file at path (through
// path.lock beside it) and returns the function that releases it. Clients
// in several processes that read, change and save the same file hold it
// so one does not undo another's write.
func lockConfig(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
