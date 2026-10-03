//go:build !unix

package cli

import "context"

func topInput(_ context.Context, _ context.CancelFunc) (func(), error) {
	return func() {}, nil
}
