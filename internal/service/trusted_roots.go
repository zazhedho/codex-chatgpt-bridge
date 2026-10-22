package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
)

const trustedRootRequestTTL = 30 * time.Minute

type TrustedRootRepository interface {
	CreateTrustedRoot(context.Context, domain.TrustedWorkspaceRoot) error
	GetTrustedRoot(context.Context, string) (domain.TrustedWorkspaceRoot, error)
	ListTrustedRoots(context.Context, time.Time) ([]domain.TrustedWorkspaceRoot, error)
	ApproveTrustedRoot(context.Context, domain.TrustedWorkspaceRoot, string, time.Time) error
	SetTrustedRootStatus(context.Context, string, string, string, time.Time) error
	EnsureTrustedWorkspace(context.Context, domain.Workspace) (domain.Workspace, error)
}

type sessionProjectBindingReader interface {
	GetApprovedSessionProjectBinding(context.Context, string) (domain.SessionProjectBinding, error)
}

type TrustedRootResolution struct {
	Root          domain.TrustedWorkspaceRoot
	RootScoped    bool
	SessionPath   string
	SessionDevice uint64
	SessionInode  uint64
	ProjectPath   string
	ProjectDevice uint64
	ProjectInode  uint64
	WorkspaceID   string
}

type TrustedWorkspaceRootService struct {
	repo       TrustedRootRepository
	bindings   sessionProjectBindingReader
	workspaces *WorkspaceService
	audit      *audit.Logger
	home       string
	codexHome  string
	now        func() time.Time
}

func NewTrustedWorkspaceRootService(repo TrustedRootRepository, workspaces *WorkspaceService, auditLogger *audit.Logger, home, codexHome string) *TrustedWorkspaceRootService {
	service := &TrustedWorkspaceRootService{repo: repo, workspaces: workspaces, audit: auditLogger, home: home, codexHome: codexHome, now: time.Now}
	if bindings, ok := repo.(sessionProjectBindingReader); ok {
		service.bindings = bindings
	}
	return service
}

func (s *TrustedWorkspaceRootService) AddLocal(ctx context.Context, path, actor string) (domain.TrustedWorkspaceRoot, error) {
	if actor == "" || s.repo == nil {
		return domain.TrustedWorkspaceRoot{}, domain.ErrNotAuthorized
	}
	rootPath, err := security.CanonicalWorkspaceRoot(path, s.home, s.codexHome)
	if err != nil {
		return domain.TrustedWorkspaceRoot{}, domain.ErrInvalidInput
	}
	id, err := newID()
	if err != nil {
		return domain.TrustedWorkspaceRoot{}, err
	}
	now := s.now().UTC()
	root := domain.TrustedWorkspaceRoot{ID: id, RootPath: rootPath.Path, RootDevice: rootPath.Device, RootInode: rootPath.Inode, Status: domain.TrustedRootPending, CreatedAt: now, ExpiresAt: now.Add(trustedRootRequestTTL)}
	if err := s.repo.CreateTrustedRoot(ctx, root); err != nil {
		return domain.TrustedWorkspaceRoot{}, err
	}
	if err := s.record(ctx, "workspace-root.requested", actor, root.ID, "pending"); err != nil {
		return root, err
	}
	return root, nil
}

func (s *TrustedWorkspaceRootService) ApproveLocal(ctx context.Context, id, actor string) (domain.TrustedWorkspaceRoot, error) {
	if actor == "" {
		return domain.TrustedWorkspaceRoot{}, domain.ErrNotAuthorized
	}
	root, err := s.repo.GetTrustedRoot(ctx, id)
	if err != nil {
		return domain.TrustedWorkspaceRoot{}, err
	}
	if root.Status != domain.TrustedRootPending || !s.now().Before(root.ExpiresAt) {
		return domain.TrustedWorkspaceRoot{}, domain.ErrConflict
	}
	if err := security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome); err != nil {
		return domain.TrustedWorkspaceRoot{}, domain.ErrNotAuthorized
	}
	if err := s.repo.ApproveTrustedRoot(ctx, root, actor, s.now().UTC()); err != nil {
		return domain.TrustedWorkspaceRoot{}, err
	}
	root.Status, root.DecidedAt, root.ApprovedBy = domain.TrustedRootApproved, s.now().UTC(), actor
	if err := s.record(ctx, "workspace-root.approved", actor, root.ID, "success"); err != nil {
		return root, err
	}
	return root, nil
}

func (s *TrustedWorkspaceRootService) ListLocal(ctx context.Context, actor string) ([]domain.TrustedWorkspaceRoot, error) {
	if actor == "" {
		return nil, domain.ErrNotAuthorized
	}
	return s.repo.ListTrustedRoots(ctx, s.now().UTC())
}

func (s *TrustedWorkspaceRootService) GetLocal(ctx context.Context, id, actor string) (domain.TrustedWorkspaceRoot, error) {
	if actor == "" {
		return domain.TrustedWorkspaceRoot{}, domain.ErrNotAuthorized
	}
	return s.repo.GetTrustedRoot(ctx, id)
}

func (s *TrustedWorkspaceRootService) VerifyLocal(ctx context.Context, id, actor string) (domain.TrustedWorkspaceRoot, error) {
	root, err := s.GetLocal(ctx, id, actor)
	if err != nil {
		return domain.TrustedWorkspaceRoot{}, err
	}
	if root.Status != domain.TrustedRootApproved || security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome) != nil {
		return domain.TrustedWorkspaceRoot{}, domain.ErrNotAuthorized
	}
	return root, nil
}

func (s *TrustedWorkspaceRootService) SetStatusLocal(ctx context.Context, id, status, actor string) error {
	if actor == "" {
		return domain.ErrNotAuthorized
	}
	root, err := s.repo.GetTrustedRoot(ctx, id)
	if err != nil {
		return err
	}
	valid := false
	switch status {
	case domain.TrustedRootDisabled:
		valid = root.Status == domain.TrustedRootApproved
	case domain.TrustedRootApproved:
		valid = root.Status == domain.TrustedRootDisabled && security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome) == nil
	case domain.TrustedRootRevoked:
		valid = root.Status == domain.TrustedRootPending || root.Status == domain.TrustedRootApproved || root.Status == domain.TrustedRootDisabled
	}
	if !valid {
		return domain.ErrConflict
	}
	if err := s.repo.SetTrustedRootStatus(ctx, id, root.Status, status, s.now().UTC()); err != nil {
		return err
	}
	return s.record(ctx, "workspace-root."+status, actor, id, "success")
}

func (s *TrustedWorkspaceRootService) ResolveSession(ctx context.Context, sessionPath string) (TrustedRootResolution, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return TrustedRootResolution{}, domain.ErrNotAuthorized
	}
	return s.resolveSession(ctx, sessionPath)
}

func (s *TrustedWorkspaceRootService) resolveSession(ctx context.Context, sessionPath string) (TrustedRootResolution, error) {
	canonicalSession, err := canonicalSessionDirectory(sessionPath)
	if err != nil {
		return TrustedRootResolution{}, domain.ErrNotAuthorized
	}
	roots, err := s.repo.ListTrustedRoots(ctx, s.now().UTC())
	if err != nil {
		return TrustedRootResolution{}, domain.ErrServiceUnavailable
	}
	var candidates []domain.TrustedWorkspaceRoot
	for i := range roots {
		root := roots[i]
		if root.Status != domain.TrustedRootApproved || !security.PathWithin(root.RootPath, canonicalSession) {
			continue
		}
		if security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome) == nil {
			if canonicalSession == root.RootPath {
				resolution := TrustedRootResolution{Root: root, RootScoped: true, SessionPath: canonicalSession, SessionDevice: root.RootDevice, SessionInode: root.RootInode, ProjectPath: root.RootPath, ProjectDevice: root.RootDevice, ProjectInode: root.RootInode, WorkspaceID: logicalWorkspaceID(root.RootPath)}
				if s.workspaces != nil {
					existing, ok, err := s.workspaces.FindStoredByRootPathLocal(ctx, root.RootPath)
					if err != nil {
						return TrustedRootResolution{}, domain.ErrServiceUnavailable
					}
					if ok {
						if existing.RootDevice != root.RootDevice || existing.RootInode != root.RootInode {
							return TrustedRootResolution{}, domain.ErrNotAuthorized
						}
						if existing.TrustedRootID != "" && existing.TrustedRootID != root.ID {
							oldRoot, err := s.repo.GetTrustedRoot(ctx, existing.TrustedRootID)
							if !existing.RootSessionOnly || err != nil || (oldRoot.Status != domain.TrustedRootDisabled && oldRoot.Status != domain.TrustedRootRevoked && oldRoot.Status != domain.TrustedRootExpired) {
								return TrustedRootResolution{}, domain.ErrNotAuthorized
							}
						}
						resolution.WorkspaceID = existing.ID
					}
				}
				return resolution, nil
			}
			candidates = append(candidates, root)
		}
	}
	if len(candidates) == 0 {
		return TrustedRootResolution{}, domain.ErrNotAuthorized
	}
	sort.SliceStable(candidates, func(i, j int) bool { return len(candidates[i].RootPath) > len(candidates[j].RootPath) })

	var resolutions []TrustedRootResolution
	for _, root := range candidates {
		projectPath, found, err := security.FindGitProjectRoot(root.RootPath, canonicalSession)
		if err != nil {
			return TrustedRootResolution{}, domain.ErrNotAuthorized
		}
		if !found {
			continue
		}
		project, err := security.CanonicalWorkspaceRoot(projectPath, s.home, s.codexHome)
		if err != nil || !security.PathWithin(root.RootPath, project.Path) {
			return TrustedRootResolution{}, domain.ErrNotAuthorized
		}
		sessionRoot, err := security.CanonicalWorkspaceRoot(canonicalSession, s.home, s.codexHome)
		if err != nil {
			return TrustedRootResolution{}, domain.ErrNotAuthorized
		}
		resolutions = append(resolutions, TrustedRootResolution{Root: root, SessionPath: canonicalSession, SessionDevice: sessionRoot.Device, SessionInode: sessionRoot.Inode, ProjectPath: project.Path, ProjectDevice: project.Device, ProjectInode: project.Inode, WorkspaceID: logicalWorkspaceID(project.Path)})
	}
	if len(resolutions) == 0 {
		return TrustedRootResolution{}, domain.ErrConflict
	}
	if s.workspaces != nil {
		var fallback *TrustedRootResolution
		var oldRootID string
		for i := range resolutions {
			existing, ok, err := s.workspaces.FindStoredByRootPathLocal(ctx, resolutions[i].ProjectPath)
			if err != nil {
				return TrustedRootResolution{}, domain.ErrServiceUnavailable
			}
			if !ok {
				continue
			}
			if existing.RootDevice != resolutions[i].ProjectDevice || existing.RootInode != resolutions[i].ProjectInode {
				return TrustedRootResolution{}, domain.ErrNotAuthorized
			}
			resolutions[i].WorkspaceID = existing.ID
			if existing.TrustedRootID == "" {
				continue
			}
			if existing.TrustedRootID == resolutions[i].Root.ID {
				return resolutions[i], nil
			}
			oldRootID = existing.TrustedRootID
			if fallback == nil {
				copy := resolutions[i]
				fallback = &copy
			}
		}
		if oldRootID != "" {
			oldRoot, err := s.repo.GetTrustedRoot(ctx, oldRootID)
			if err != nil || (oldRoot.Status != domain.TrustedRootDisabled && oldRoot.Status != domain.TrustedRootRevoked && oldRoot.Status != domain.TrustedRootExpired) {
				return TrustedRootResolution{}, domain.ErrNotAuthorized
			}
		}
		if fallback != nil {
			return *fallback, nil
		}
	}
	return resolutions[0], nil
}

func (s *TrustedWorkspaceRootService) ProjectChoices(ctx context.Context, sessionPath string, offset, limit int) ([]domain.SessionProjectChoice, *int, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, nil, domain.ErrNotAuthorized
	}
	choices, err := s.projectChoices(ctx, sessionPath)
	if err != nil {
		return nil, nil, err
	}
	if offset < 0 || limit < 1 || limit > 50 || offset > len(choices) {
		return nil, nil, domain.ErrInvalidInput
	}
	end := min(offset+limit, len(choices))
	page := make([]domain.SessionProjectChoice, 0, end-offset)
	for _, choice := range choices[offset:end] {
		page = append(page, choice.choice)
	}
	var next *int
	if end < len(choices) {
		next = &end
	}
	return page, next, nil
}

type sessionProjectCandidate struct {
	choice     domain.SessionProjectChoice
	resolution TrustedRootResolution
}

func (s *TrustedWorkspaceRootService) resolveProjectChoice(ctx context.Context, sessionPath, projectID string) (TrustedRootResolution, error) {
	choices, err := s.projectChoices(ctx, sessionPath)
	if err != nil {
		return TrustedRootResolution{}, err
	}
	for _, choice := range choices {
		if choice.choice.ProjectID == projectID {
			return choice.resolution, nil
		}
	}
	return TrustedRootResolution{}, domain.ErrNotAuthorized
}

func (s *TrustedWorkspaceRootService) projectChoices(ctx context.Context, sessionPath string) ([]sessionProjectCandidate, error) {
	canonicalSession, err := canonicalSessionDirectory(sessionPath)
	if err != nil {
		return nil, domain.ErrNotAuthorized
	}
	sessionRoot, err := security.CanonicalWorkspaceRoot(canonicalSession, s.home, s.codexHome)
	if err != nil || sessionRoot.Path != canonicalSession {
		return nil, domain.ErrNotAuthorized
	}
	roots, err := s.repo.ListTrustedRoots(ctx, s.now().UTC())
	if err != nil {
		return nil, domain.ErrServiceUnavailable
	}
	var trusted []domain.TrustedWorkspaceRoot
	for _, root := range roots {
		if root.Status == domain.TrustedRootApproved && security.PathWithin(root.RootPath, canonicalSession) &&
			security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome) == nil {
			trusted = append(trusted, root)
		}
	}
	if len(trusted) == 0 {
		return nil, domain.ErrNotAuthorized
	}
	sort.SliceStable(trusted, func(i, j int) bool { return len(trusted[i].RootPath) > len(trusted[j].RootPath) })
	root := trusted[0]
	projects, err := security.FindGitProjectRoots(root.RootPath)
	if err != nil {
		return nil, domain.ErrServiceUnavailable
	}
	choices := make([]sessionProjectCandidate, 0, len(projects))
	for _, projectPath := range projects {
		project, err := security.CanonicalWorkspaceRoot(projectPath, s.home, s.codexHome)
		if err != nil || project.Path == root.RootPath || !security.PathWithin(root.RootPath, project.Path) {
			continue
		}
		marker, err := security.IsGitProjectRoot(project.Path)
		if err != nil || !marker {
			continue
		}
		relative, err := filepath.Rel(root.RootPath, project.Path)
		if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		resolution := TrustedRootResolution{Root: root, SessionPath: canonicalSession, SessionDevice: sessionRoot.Device, SessionInode: sessionRoot.Inode, ProjectPath: project.Path, ProjectDevice: project.Device, ProjectInode: project.Inode, WorkspaceID: logicalWorkspaceID(project.Path)}
		choices = append(choices, sessionProjectCandidate{choice: domain.SessionProjectChoice{ProjectID: projectChoiceID(root.ID, project.Path), Name: sanitizeCatalogText(filepath.Base(project.Path), 100), RelativePath: sanitizeCatalogText(relative, 180)}, resolution: resolution})
	}
	return choices, nil
}

func (s *TrustedWorkspaceRootService) ValidateProjectBinding(ctx context.Context, binding domain.SessionProjectBinding, session domain.CodexSession, workspace domain.Workspace) error {
	sessionPath, pathErr := canonicalSessionDirectory(session.WorkspacePath)
	// Codex can report the explicitly selected repository as cwd for a parent-root thread.
	if binding.Status != domain.SessionProjectBindingApproved || binding.ThreadID != session.ID || binding.WorkspaceID != workspace.ID ||
		pathErr != nil || (binding.SessionPath != sessionPath && binding.ProjectPath != sessionPath) || !binding.SessionCreatedAt.Equal(session.CreatedAt) || !session.IsCodexCLISession() ||
		binding.ProjectPath != workspace.RootPath || binding.ProjectDevice != workspace.RootDevice || binding.ProjectInode != workspace.RootInode ||
		(binding.TrustedRootID != workspace.TrustedRootID && workspace.TrustedRootID != "") {
		return domain.ErrNotAuthorized
	}
	root, err := s.repo.GetTrustedRoot(ctx, binding.TrustedRootID)
	if err != nil || root.Status != domain.TrustedRootApproved || root.RootPath != binding.TrustedRootPath || root.RootDevice != binding.RootDevice || root.RootInode != binding.RootInode ||
		security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome) != nil {
		return domain.ErrNotAuthorized
	}
	sessionRoot, err := security.CanonicalWorkspaceRoot(binding.SessionPath, s.home, s.codexHome)
	if err != nil || sessionRoot.Path != binding.SessionPath || sessionRoot.Device != binding.SessionDevice || sessionRoot.Inode != binding.SessionInode || !security.PathWithin(root.RootPath, sessionRoot.Path) {
		return domain.ErrNotAuthorized
	}
	project, err := security.CanonicalWorkspaceRoot(binding.ProjectPath, s.home, s.codexHome)
	if err != nil || project.Path != binding.ProjectPath || project.Device != binding.ProjectDevice || project.Inode != binding.ProjectInode || project.Path == root.RootPath || !security.PathWithin(root.RootPath, project.Path) || projectChoiceID(root.ID, project.Path) != binding.ProjectID {
		return domain.ErrNotAuthorized
	}
	marker, err := security.IsGitProjectRoot(project.Path)
	if err != nil || !marker {
		return domain.ErrNotAuthorized
	}
	return nil
}

func projectChoiceID(rootID, projectPath string) string {
	hash := sha256.Sum256([]byte(rootID + "\x00" + filepath.Clean(projectPath)))
	return hex.EncodeToString(hash[:16])
}

func (s *TrustedWorkspaceRootService) ValidateThread(ctx context.Context, thread domain.Thread, workspace domain.Workspace) error {
	if thread.Source == domain.ThreadSourceCodexCLIProjectBound {
		if s.bindings == nil || thread.WorkspaceID != workspace.ID || thread.TrustedRootID == "" {
			return domain.ErrNotAuthorized
		}
		binding, err := s.bindings.GetApprovedSessionProjectBinding(ctx, thread.ID)
		if err != nil || binding.TrustedRootID != thread.TrustedRootID || binding.SessionPath != thread.SessionPath {
			return domain.ErrNotAuthorized
		}
		session := domain.CodexSession{ID: thread.ID, WorkspacePath: binding.SessionPath, Source: "cli", CreatedAt: binding.SessionCreatedAt}
		return s.ValidateProjectBinding(ctx, binding, session, workspace)
	}
	if thread.Source != domain.ThreadSourceCodexCLIRoot || thread.TrustedRootID == "" || thread.WorkspaceID != workspace.ID {
		return domain.ErrNotAuthorized
	}
	resolution, err := s.resolveSession(ctx, thread.SessionPath)
	if err != nil || resolution.Root.ID != thread.TrustedRootID || resolution.WorkspaceID != workspace.ID || resolution.ProjectPath != workspace.RootPath ||
		resolution.ProjectDevice != workspace.RootDevice || resolution.ProjectInode != workspace.RootInode ||
		(workspace.TrustedRootID != "" && workspace.TrustedRootID != thread.TrustedRootID) {
		return domain.ErrNotAuthorized
	}
	return nil
}

func (s *TrustedWorkspaceRootService) EnsureWorkspace(ctx context.Context, resolution TrustedRootResolution) (domain.Workspace, error) {
	if err := s.validateResolution(ctx, resolution); err != nil {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	workspace := domain.Workspace{ID: resolution.WorkspaceID, RootPath: resolution.ProjectPath, RootDevice: resolution.ProjectDevice, RootInode: resolution.ProjectInode, CreatedAt: s.now().UTC(), TrustedRootID: resolution.Root.ID, RootSessionOnly: true}
	workspace, err := s.repo.EnsureTrustedWorkspace(ctx, workspace)
	if err != nil {
		return domain.Workspace{}, err
	}
	if workspace.ID != resolution.WorkspaceID || workspace.RootPath != resolution.ProjectPath || workspace.RootDevice != resolution.ProjectDevice || workspace.RootInode != resolution.ProjectInode ||
		(workspace.TrustedRootID != "" && workspace.TrustedRootID != resolution.Root.ID) ||
		security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: workspace.RootPath, Device: workspace.RootDevice, Inode: workspace.RootInode}, s.home, s.codexHome) != nil {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	return workspace, nil
}

func (s *TrustedWorkspaceRootService) validateResolution(ctx context.Context, resolution TrustedRootResolution) error {
	root, err := s.repo.GetTrustedRoot(ctx, resolution.Root.ID)
	if err != nil || root.Status != domain.TrustedRootApproved || root.RootPath != resolution.Root.RootPath || root.RootDevice != resolution.Root.RootDevice || root.RootInode != resolution.Root.RootInode ||
		security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome) != nil {
		return domain.ErrNotAuthorized
	}
	session, err := security.CanonicalWorkspaceRoot(resolution.SessionPath, s.home, s.codexHome)
	if err != nil || session.Path != resolution.SessionPath || session.Device != resolution.SessionDevice || session.Inode != resolution.SessionInode || !security.PathWithin(root.RootPath, session.Path) {
		return domain.ErrNotAuthorized
	}
	project, err := security.CanonicalWorkspaceRoot(resolution.ProjectPath, s.home, s.codexHome)
	if err != nil || project.Path != resolution.ProjectPath || project.Device != resolution.ProjectDevice || project.Inode != resolution.ProjectInode || !security.PathWithin(root.RootPath, project.Path) {
		return domain.ErrNotAuthorized
	}
	if resolution.RootScoped {
		if session.Path != root.RootPath || project.Path != root.RootPath || project.Device != root.RootDevice || project.Inode != root.RootInode {
			return domain.ErrNotAuthorized
		}
		return nil
	}
	marker, err := security.IsGitProjectRoot(project.Path)
	if err != nil || !marker {
		return domain.ErrNotAuthorized
	}
	if session.Path != root.RootPath {
		actual, found, err := security.FindGitProjectRoot(root.RootPath, session.Path)
		if err != nil || !found || actual != project.Path {
			return domain.ErrNotAuthorized
		}
	}
	return nil
}

func (s *TrustedWorkspaceRootService) record(ctx context.Context, operation, actor, rootID, outcome string) error {
	if s.audit == nil {
		return nil
	}
	if err := s.audit.Record(ctx, audit.Event{Operation: operation, Principal: "local:" + actor, ResourceID: rootID, Outcome: outcome}); err != nil {
		return domain.ErrServiceUnavailable
	}
	return nil
}

func canonicalSessionDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("session path is not canonical")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("session path is unavailable")
	}
	return filepath.Abs(canonical)
}

func logicalWorkspaceID(path string) string {
	hash := sha256.Sum256([]byte(filepath.Clean(path)))
	return "root-" + hex.EncodeToString(hash[:12])
}
