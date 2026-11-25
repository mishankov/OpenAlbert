//go:build !unix

package bridge

import "errors"

// FileLock is a placeholder on platforms without flock support.
type FileLock struct{}

// AcquireLock reports bridge singleton locking as unsupported on non-Unix platforms
// so the caller falls back cleanly to interactive-only ACP mode.
func AcquireLock(string) (*FileLock, bool, error) {
	return nil, false, errors.New("bridge singleton file lock is unsupported on this platform")
}

// Release is a no-op for the unsupported lock stub.
func (*FileLock) Release() error { return nil }
