//go:build !unix

package backup

// acquireLock is a no-op on platforms without flock; the in-process mutex of
// the service still prevents concurrent runs.
func acquireLock(dest string) (func(), error) {
	return func() {}, nil
}
