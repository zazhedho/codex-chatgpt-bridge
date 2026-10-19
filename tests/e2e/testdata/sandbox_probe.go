package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

func errno(err error) syscall.Errno {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		if code, ok := pathErr.Err.(syscall.Errno); ok {
			return code
		}
	}
	return 0
}

func denied(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

func main() {
	if len(os.Args) != 5 {
		fmt.Println("usage_error")
		os.Exit(2)
	}
	inside, sibling, writePath, marker := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	fmt.Printf("invocation=%s\n", marker)

	allowed, allowedErr := os.ReadFile(inside)
	if allowedErr != nil {
		fmt.Printf("allowed_read=denied errno=%d\n", errno(allowedErr))
	} else {
		fmt.Printf("allowed_read=ok value=%s\n", strings.TrimSpace(string(allowed)))
	}

	outside, siblingErr := os.ReadFile(sibling)
	if siblingErr != nil {
		fmt.Printf("sibling_read=denied errno=%d\n", errno(siblingErr))
	} else {
		fmt.Printf("sibling_read=allowed value=%s\n", strings.TrimSpace(string(outside)))
	}

	file, writeErr := os.OpenFile(writePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if writeErr != nil {
		fmt.Printf("workspace_write=denied errno=%d\n", errno(writeErr))
	} else {
		_, _ = file.WriteString("unexpected\n")
		_ = file.Close()
		fmt.Println("workspace_write=allowed")
	}

	if allowedErr != nil || !denied(siblingErr) || !denied(writeErr) {
		os.Exit(3)
	}
}
