//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package mcpserver

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
)

func readHostKey(path string) ([]byte, error) {
	invalid := errors.New("unsafe native host key")
	parent := filepath.Dir(path)
	if actual, err := filepath.EvalSymlinks(parent); err != nil || actual != parent {
		return nil, invalid
	}
	dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, invalid
	}
	defer unix.Close(dir)
	var info unix.Stat_t
	if unix.Fstat(dir, &info) != nil || info.Uid != uint32(os.Getuid()) || info.Mode&0077 != 0 {
		return nil, invalid
	}
	fd, err := unix.Openat(dir, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, invalid
	}
	f := os.NewFile(uintptr(fd), "host-key")
	defer f.Close()
	if unix.Fstat(fd, &info) != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || info.Mode&0777 != 0600 || info.Nlink != 1 {
		return nil, invalid
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		return nil, invalid
	}
	return key, nil
}
