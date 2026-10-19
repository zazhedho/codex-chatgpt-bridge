//go:build !darwin

package security

import (
	"os"
	"syscall"
)

func fileIdentity(os.FileInfo) (uint64, uint64, error) {
	return 0, 0, syscall.ENOTSUP
}
