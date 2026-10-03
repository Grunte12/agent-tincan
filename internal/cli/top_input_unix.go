//go:build unix

package cli

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func topInput(ctx context.Context, cancel context.CancelFunc) (func(), error) {
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var key [1]byte
		for {
			if ctx.Err() != nil {
				return
			}
			fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
			n, err := unix.Poll(fds, 100)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				cancel()
				return
			}
			if n == 0 {
				continue
			}
			n, err = unix.Read(int(os.Stdin.Fd()), key[:])
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			if err != nil || n == 0 {
				cancel()
				return
			}
			if key[0] == 'q' || key[0] == 3 {
				cancel()
				return
			}
		}
	}()
	return func() { cancel(); <-done; _ = term.Restore(int(os.Stdin.Fd()), state) }, nil
}
