package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func errno(err error) syscall.Errno {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		var code syscall.Errno
		if errors.As(pathErr.Err, &code) {
			return code
		}
	}
	return 0
}

func denied(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

func read(label, path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("%s=denied errno=%d\n", label, errno(err))
		return err
	}
	fmt.Printf("%s=ok value=%s\n", label, content)
	return nil
}

func main() {
	if len(os.Args) != 7 {
		fmt.Println("usage_error")
		os.Exit(2)
	}
	one, two, otherRoot, symlink, writePath, invocation := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6]
	fmt.Printf("invocation=%s\n", invocation)
	oneErr := read("repo_one", one)
	twoErr := read("repo_two", two)
	otherErr := read("other_root", otherRoot)
	symlinkErr := read("symlink", symlink)
	writeErr := os.WriteFile(writePath, []byte("must not be created\n"), 0o600)
	if writeErr != nil {
		fmt.Printf("write=denied errno=%d\n", errno(writeErr))
	} else {
		fmt.Println("write=allowed")
	}
	if oneErr != nil || twoErr != nil || !denied(otherErr) || !denied(symlinkErr) || !denied(writeErr) {
		os.Exit(3)
	}
}
