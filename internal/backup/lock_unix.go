//go:build unix

package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const lockName = ".my_sync.lock"

// acquireLock takes an exclusive, non-blocking flock on dest so that the web
// service and a CLI run never write into the same destination concurrently.
// The kernel drops the lock if the process dies.
func acquireLock(dest string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dest, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("目标目录 %s 正被另一个备份进程使用", dest)
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
