//go:build darwin

package security

import (
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, syscall.EINVAL
	}
	return uint64(stat.Dev), stat.Ino, nil
}
