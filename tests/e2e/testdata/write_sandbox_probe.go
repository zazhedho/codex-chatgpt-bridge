package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func errorNumber(err error) syscall.Errno {
	var code syscall.Errno
	if errors.As(err, &code) {
		return code
	}
	return 0
}

func denied(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

func main() {
	if len(os.Args) != 6 {
		fmt.Println("usage_error")
		os.Exit(2)
	}
	inside, sibling, outsideWrite, symlinkWrite, marker := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]
	fmt.Printf("invocation=%s\n", marker)
	_, insideReadErr := os.ReadFile(inside)
	if insideReadErr != nil {
		fmt.Printf("inside_read=denied errno=%d\n", errorNumber(insideReadErr))
	} else {
		fmt.Println("inside_read=allowed")
	}
	_, siblingReadErr := os.ReadFile(sibling)
	fmt.Printf("outside_read=denied errno=%d\n", errorNumber(siblingReadErr))
	if siblingReadErr == nil {
		fmt.Println("outside_read=allowed")
	}
	insideWritePath := filepath.Join(filepath.Dir(inside), "workspace-write-probe.txt")
	insideWriteErr := os.WriteFile(insideWritePath, []byte("allowed fixture write\n"), 0o600)
	if insideWriteErr == nil {
		fmt.Println("inside_write=allowed")
	} else {
		fmt.Printf("inside_write=denied errno=%d\n", errorNumber(insideWriteErr))
	}
	outsideWriteErr := os.WriteFile(outsideWrite, []byte("must not be written\n"), 0o600)
	fmt.Printf("outside_write=denied errno=%d\n", errorNumber(outsideWriteErr))
	if outsideWriteErr == nil {
		fmt.Println("outside_write=allowed")
	}
	symlinkWriteErr := os.WriteFile(symlinkWrite, []byte("must not escape\n"), 0o600)
	fmt.Printf("symlink_write=denied errno=%d\n", errorNumber(symlinkWriteErr))
	if symlinkWriteErr == nil {
		fmt.Println("symlink_write=allowed")
	}
	connection, networkErr := net.DialTimeout("tcp", "1.1.1.1:443", 2*time.Second)
	if networkErr != nil {
		fmt.Printf("network=denied errno=%d\n", errorNumber(networkErr))
	} else {
		_ = connection.Close()
		fmt.Println("network=allowed")
	}
	if insideReadErr != nil || siblingReadErr == nil || insideWriteErr != nil || !denied(outsideWriteErr) || !denied(symlinkWriteErr) || !denied(networkErr) {
		os.Exit(3)
	}
}
