package security

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type WorkspaceRoot struct {
	Path   string
	Device uint64
	Inode  uint64
}

func CanonicalWorkspaceRoot(input, homeDir, codexHome string) (WorkspaceRoot, error) {
	if !filepath.IsAbs(input) || hasTraversal(input) {
		return WorkspaceRoot{}, errors.New("workspace path must be absolute without traversal")
	}
	canonical, err := filepath.EvalSymlinks(input)
	if err != nil {
		return WorkspaceRoot{}, errors.New("workspace root is unavailable")
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return WorkspaceRoot{}, errors.New("workspace root is invalid")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return WorkspaceRoot{}, errors.New("workspace root must be a directory")
	}
	if isUnsafeWorkspaceRoot(canonical, canonicalOptionalPath(homeDir), canonicalOptionalPath(codexHome)) {
		return WorkspaceRoot{}, errors.New("workspace root is protected")
	}
	device, inode, err := fileIdentity(info)
	if err != nil {
		return WorkspaceRoot{}, errors.New("workspace identity is unavailable")
	}
	return WorkspaceRoot{Path: canonical, Device: device, Inode: inode}, nil
}

func RevalidateWorkspaceRoot(root WorkspaceRoot, homeDir, codexHome string) error {
	current, err := CanonicalWorkspaceRoot(root.Path, homeDir, codexHome)
	if err != nil || current.Path != root.Path || current.Device != root.Device || current.Inode != root.Inode {
		return errors.New("workspace root changed or is unavailable")
	}
	return nil
}

func ResolveWorkspacePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || hasTraversal(relative) || strings.ContainsRune(relative, 0) {
		return "", errors.New("path must be relative to the workspace")
	}
	cleanRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errors.New("workspace root is invalid")
	}
	cleanRoot, err = filepath.Abs(cleanRoot)
	if err != nil {
		return "", errors.New("workspace root is invalid")
	}
	candidate := filepath.Join(cleanRoot, relative)
	if !pathWithin(cleanRoot, candidate) {
		return "", errors.New("path escapes the workspace")
	}
	check := candidate
	for {
		resolved, resolveErr := filepath.EvalSymlinks(check)
		if resolveErr == nil {
			resolved, resolveErr = filepath.Abs(resolved)
			if resolveErr != nil || !pathWithin(cleanRoot, resolved) {
				return "", errors.New("path escapes the workspace")
			}
			break
		}
		if !os.IsNotExist(resolveErr) {
			return "", errors.New("workspace path is unavailable")
		}
		parent := filepath.Dir(check)
		if parent == check {
			return "", errors.New("workspace path is unavailable")
		}
		check = parent
	}
	return candidate, nil
}

func isUnsafeWorkspaceRoot(root, homeDir, codexHome string) bool {
	if root == string(filepath.Separator) ||
		(homeDir != "" && pathWithin(root, homeDir)) ||
		(codexHome != "" && (pathWithin(root, codexHome) || pathWithin(codexHome, root))) {
		return true
	}
	for _, part := range strings.Split(filepath.Clean(root), string(filepath.Separator)) {
		switch strings.ToLower(part) {
		case ".ssh", ".aws", ".kube", ".codex", "keychains":
			return true
		}
	}
	return false
}

func canonicalOptionalPath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return canonical
	}
	return filepath.Clean(absolute)
}

func hasTraversal(value string) bool {
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return true
		}
	}
	return false
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)))
}
