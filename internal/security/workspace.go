package security

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
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

func PathWithin(root, target string) bool { return pathWithin(root, target) }

// FindGitProjectRoot returns the nearest repository marker without following .git pointers.
func FindGitProjectRoot(trustedRoot, sessionPath string) (string, bool, error) {
	root, err := canonicalExistingDirectory(trustedRoot)
	if err != nil {
		return "", false, err
	}
	current, err := canonicalExistingDirectory(sessionPath)
	if err != nil || !pathWithin(root, current) {
		return "", false, errors.New("session path is outside the trusted root")
	}
	for {
		found, err := hasGitRepositoryMarker(current)
		if err != nil {
			return "", false, err
		}
		if found {
			return current, true, nil
		}
		if current == root {
			return "", false, nil
		}
		parent := filepath.Dir(current)
		if parent == current || !pathWithin(root, parent) {
			return "", false, nil
		}
		current = parent
	}
}

// FindGitProjectRoots returns Git boundaries below a trusted parent; it never follows symlinks.
func FindGitProjectRoots(trustedRoot string) ([]string, error) {
	root, err := canonicalExistingDirectory(trustedRoot)
	if err != nil {
		return nil, err
	}
	const maxDirectories, maxProjects = 25000, 500
	visited := 0
	projects := make([]string, 0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		visited++
		if visited > maxDirectories {
			return errors.New("trusted root contains too many directories to enumerate safely")
		}
		if entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		found, err := hasGitRepositoryMarker(path)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		canonical, err := canonicalExistingDirectory(path)
		if err != nil || !pathWithin(root, canonical) {
			return errors.New("repository boundary changed during enumeration")
		}
		projects = append(projects, canonical)
		if len(projects) > maxProjects {
			return errors.New("trusted root contains too many repositories to enumerate safely")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(projects, func(i, j int) bool {
		return filepath.Clean(projects[i]) < filepath.Clean(projects[j])
	})
	return projects, nil
}

func IsGitProjectRoot(path string) (bool, error) {
	canonical, err := canonicalExistingDirectory(path)
	if err != nil || canonical != path {
		return false, errors.New("project path is not canonical")
	}
	return hasGitRepositoryMarker(canonical)
}

func hasGitRepositoryMarker(directory string) (bool, error) {
	marker := filepath.Join(directory, ".git")
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	if info.IsDir() {
		return true, nil
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	file, err := os.Open(marker)
	if err != nil {
		return false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if readErr != nil {
		return false, readErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	value := strings.TrimSpace(string(data))
	return len(data) <= 4096 && !strings.ContainsRune(value, 0) && strings.HasPrefix(value, "gitdir:") && strings.TrimSpace(strings.TrimPrefix(value, "gitdir:")) != "", nil
}

func canonicalExistingDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || hasTraversal(path) {
		return "", errors.New("path must be absolute and canonical")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("path is unavailable")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("path is invalid")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("path must be a directory")
	}
	return resolved, nil
}
