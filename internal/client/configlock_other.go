//go:build !unix

package client

// lockConfig is a no-op where flock is not available; config writes there
// are not serialized between processes.
func lockConfig(string) (func(), error) { return func() {}, nil }
