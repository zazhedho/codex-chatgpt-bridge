package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
)

var workspaceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

type WorkspaceRepository interface {
	ListWorkspaces(context.Context) ([]domain.Workspace, error)
	ListWorkspacesPage(context.Context, int, int) ([]domain.Workspace, bool, error)
	HasWorkspaceConflict(context.Context, string, string, time.Time) (bool, error)
	GetWorkspace(context.Context, string) (domain.Workspace, error)
	GetWorkspaceByRootPath(context.Context, string) (domain.Workspace, error)
	InsertWorkspaceRequest(context.Context, domain.WorkspaceRequest) error
	ListPendingWorkspaceRequests(context.Context, time.Time) ([]domain.WorkspaceRequest, error)
	ListPendingWorkspaceRequestsPage(context.Context, time.Time, int, int) ([]domain.WorkspaceRequest, bool, error)
	GetPendingWorkspaceRequest(context.Context, string, time.Time) (domain.WorkspaceRequest, error)
	GetWorkspaceRequest(context.Context, string) (domain.WorkspaceRequest, error)
	ApproveWorkspaceRequest(context.Context, string, domain.Workspace) error
	SetWorkspaceRequestStatus(context.Context, string, string) error
	SetWorkspaceWriteEnabled(context.Context, string, bool) error
	SetWorkspaceSessionTrusted(context.Context, domain.Workspace, bool) error
}

type TrustedRootLookup interface {
	GetTrustedRoot(context.Context, string) (domain.TrustedWorkspaceRoot, error)
	ListTrustedRoots(context.Context, time.Time) ([]domain.TrustedWorkspaceRoot, error)
}

type WorkspaceService struct {
	repo       WorkspaceRepository
	audit      *audit.Logger
	home       string
	codexHome  string
	rootLookup TrustedRootLookup
	now        func() time.Time
}

func NewWorkspaceService(repo WorkspaceRepository, auditLogger *audit.Logger, home, codexHome string, rootLookups ...TrustedRootLookup) *WorkspaceService {
	var rootLookup TrustedRootLookup
	if len(rootLookups) != 0 {
		rootLookup = rootLookups[0]
	}
	return &WorkspaceService{repo: repo, audit: auditLogger, home: home, codexHome: codexHome, rootLookup: rootLookup, now: time.Now}
}

func (s *WorkspaceService) List(ctx context.Context) ([]domain.Workspace, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, domain.ErrNotAuthorized
	}
	return s.list(ctx)
}

func (s *WorkspaceService) ListPage(ctx context.Context, limit, offset int) ([]domain.Workspace, *int, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, nil, domain.ErrNotAuthorized
	}
	limit, offset, err := pageBounds(limit, offset)
	if err != nil {
		return nil, nil, err
	}
	workspaces, hasMore, err := s.repo.ListWorkspacesPage(ctx, limit, offset)
	if err != nil {
		return nil, nil, err
	}
	valid := make([]domain.Workspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		if s.validWorkspace(ctx, workspace) == nil {
			valid = append(valid, workspace)
		}
	}
	return valid, pageNextOffset(offset, limit, hasMore), nil
}

func (s *WorkspaceService) ListLocal(ctx context.Context) ([]domain.Workspace, error) {
	return s.list(ctx)
}

func (s *WorkspaceService) list(ctx context.Context) ([]domain.Workspace, error) {
	workspaces, err := s.repo.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	valid := make([]domain.Workspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		if err := s.validWorkspace(ctx, workspace); err != nil {
			continue
		}
		valid = append(valid, workspace)
	}
	return valid, nil
}

func (s *WorkspaceService) Get(ctx context.Context, id string) (domain.Workspace, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	return s.GetLocal(ctx, id)
}

func (s *WorkspaceService) GetLocal(ctx context.Context, id string) (domain.Workspace, error) {
	if !workspaceIDPattern.MatchString(id) {
		return domain.Workspace{}, domain.ErrInvalidInput
	}
	workspace, err := s.repo.GetWorkspace(ctx, id)
	if err != nil {
		return domain.Workspace{}, err
	}
	if err := s.validWorkspace(ctx, workspace); err != nil {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	return workspace, nil
}

func (s *WorkspaceService) FindByRootPathLocal(ctx context.Context, rootPath string) (domain.Workspace, bool, error) {
	workspaces, err := s.list(ctx)
	if err != nil {
		return domain.Workspace{}, false, err
	}
	for _, workspace := range workspaces {
		if workspace.RootPath == rootPath {
			return workspace, true, nil
		}
	}
	return domain.Workspace{}, false, nil
}

// FindStoredByRootPathLocal returns an existing database row for a canonical root path without authorizing it.
func (s *WorkspaceService) FindStoredByRootPathLocal(ctx context.Context, rootPath string) (domain.Workspace, bool, error) {
	workspace, err := s.repo.GetWorkspaceByRootPath(ctx, rootPath)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Workspace{}, false, nil
	}
	if err != nil {
		return domain.Workspace{}, false, err
	}
	return workspace, true, nil
}

func (s *WorkspaceService) validWorkspace(ctx context.Context, workspace domain.Workspace) error {
	if err := security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: workspace.RootPath, Device: workspace.RootDevice, Inode: workspace.RootInode}, s.home, s.codexHome); err != nil {
		return domain.ErrNotAuthorized
	}
	if workspace.TrustedRootID == "" {
		return nil
	}
	if s.rootLookup == nil {
		return domain.ErrNotAuthorized
	}
	root, err := s.rootLookup.GetTrustedRoot(ctx, workspace.TrustedRootID)
	if err != nil || root.Status != domain.TrustedRootApproved || !security.PathWithin(root.RootPath, workspace.RootPath) || security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: root.RootPath, Device: root.RootDevice, Inode: root.RootInode}, s.home, s.codexHome) != nil {
		return domain.ErrNotAuthorized
	}
	return nil
}

func (s *WorkspaceService) SetWriteEnabledLocal(ctx context.Context, id string, enabled bool, actor string) error {
	if actor == "" {
		return domain.ErrNotAuthorized
	}
	workspace, err := s.GetLocal(ctx, id)
	if err != nil {
		return err
	}
	if enabled {
		if err := s.ValidateWriteScope(ctx, workspace); err != nil {
			return err
		}
	}
	if err := s.repo.SetWorkspaceWriteEnabled(ctx, workspace.ID, enabled); err != nil {
		return err
	}
	if s.audit != nil {
		operation, outcome := "workspace.write-disabled", "success"
		if enabled {
			operation = "workspace.write-enabled"
		}
		if err := s.audit.Record(ctx, audit.Event{Operation: operation, Principal: "local:" + actor, WorkspaceID: workspace.ID, Outcome: outcome}); err != nil {
			return fmt.Errorf("workspace write policy changed but audit write failed: %w", err)
		}
	}
	return nil
}

// ValidateWriteScope prevents a workspace that is itself an approved trusted root from becoming a broad writable root.
func (s *WorkspaceService) ValidateWriteScope(ctx context.Context, workspace domain.Workspace) error {
	if s.rootLookup == nil {
		return nil
	}
	roots, err := s.rootLookup.ListTrustedRoots(ctx, s.now().UTC())
	if err != nil {
		return domain.ErrServiceUnavailable
	}
	for _, root := range roots {
		if root.Status == domain.TrustedRootApproved && root.RootPath == workspace.RootPath {
			return domain.ErrNotAuthorized
		}
	}
	return nil
}

func (s *WorkspaceService) SetSessionTrustedLocal(ctx context.Context, id string, trusted bool, actor string) error {
	if actor == "" {
		return domain.ErrNotAuthorized
	}
	workspace, err := s.GetLocal(ctx, id)
	if err != nil {
		return err
	}
	if trusted && workspace.RootSessionOnly {
		return domain.ErrNotAuthorized
	}
	if err := s.repo.SetWorkspaceSessionTrusted(ctx, workspace, trusted); err != nil {
		return err
	}
	if s.audit != nil {
		operation := "session.workspace-trust-revoked"
		if trusted {
			operation = "session.workspace-trusted"
		}
		if err := s.audit.Record(ctx, audit.Event{Operation: operation, Principal: "local:" + actor, WorkspaceID: workspace.ID, Outcome: "success"}); err != nil {
			return fmt.Errorf("workspace session trust changed but audit write failed: %w", err)
		}
	}
	return nil
}

func (s *WorkspaceService) RequestRegistration(ctx context.Context, id, inputPath string) (domain.WorkspaceRequest, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.WorkspaceRequest{}, domain.ErrNotAuthorized
	}
	return s.requestRegistration(ctx, id, inputPath, "mcp-bearer")
}

func (s *WorkspaceService) RequestRegistrationLocal(ctx context.Context, id, inputPath, actor string) (domain.WorkspaceRequest, error) {
	if actor == "" {
		return domain.WorkspaceRequest{}, domain.ErrNotAuthorized
	}
	return s.requestRegistration(ctx, id, inputPath, "local:"+actor)
}

func (s *WorkspaceService) requestRegistration(ctx context.Context, id, inputPath, principal string) (domain.WorkspaceRequest, error) {
	if !workspaceIDPattern.MatchString(id) || len(inputPath) == 0 || len(inputPath) > 4096 {
		return domain.WorkspaceRequest{}, domain.ErrInvalidInput
	}
	root, err := security.CanonicalWorkspaceRoot(inputPath, s.home, s.codexHome)
	if err != nil {
		return domain.WorkspaceRequest{}, domain.ErrInvalidInput
	}
	conflict, err := s.repo.HasWorkspaceConflict(ctx, id, root.Path, s.now())
	if err != nil {
		return domain.WorkspaceRequest{}, err
	}
	if conflict {
		return domain.WorkspaceRequest{}, domain.ErrConflict
	}
	requestID, err := newID()
	if err != nil {
		return domain.WorkspaceRequest{}, err
	}
	request := domain.WorkspaceRequest{ID: requestID, WorkspaceID: id, RootPath: root.Path, Status: "pending", CreatedAt: s.now().UTC(), ExpiresAt: s.now().UTC().Add(30 * time.Minute)}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "workspace.registration.requested", Principal: principal, WorkspaceID: id, Outcome: "requested"}); err != nil {
			return domain.WorkspaceRequest{}, domain.ErrServiceUnavailable
		}
	}
	if err := s.repo.InsertWorkspaceRequest(ctx, request); err != nil {
		return domain.WorkspaceRequest{}, err
	}
	return request, nil
}

// ApproveRegistration is called only by the local admin CLI, never by an MCP tool.
func (s *WorkspaceService) ApproveRegistration(ctx context.Context, requestID, localActor string) (domain.Workspace, error) {
	if localActor == "" {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	request, err := s.repo.GetWorkspaceRequest(ctx, requestID)
	if err != nil {
		return domain.Workspace{}, err
	}
	if request.Status != "pending" {
		return domain.Workspace{}, domain.ErrConflict
	}
	if !s.now().Before(request.ExpiresAt) {
		_ = s.repo.SetWorkspaceRequestStatus(ctx, request.ID, "expired")
		return domain.Workspace{}, domain.ErrConflict
	}
	root, err := security.CanonicalWorkspaceRoot(request.RootPath, s.home, s.codexHome)
	if err != nil || root.Path != request.RootPath {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	workspace := domain.Workspace{ID: request.WorkspaceID, RootPath: root.Path, RootDevice: root.Device, RootInode: root.Inode, CreatedAt: s.now().UTC()}
	if _, err := os.Stat(filepath.Join(root.Path, ".git")); err == nil {
		workspace.GitRepository = root.Path
	}
	if err := s.repo.ApproveWorkspaceRequest(ctx, request.ID, workspace); err != nil {
		return domain.Workspace{}, err
	}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "workspace.registration.approved", Principal: "local:" + localActor, WorkspaceID: workspace.ID, Outcome: "success"}); err != nil {
			return domain.Workspace{}, fmt.Errorf("approval stored but audit write failed: %w", err)
		}
	}
	return workspace, nil
}

func (s *WorkspaceService) RejectRegistration(ctx context.Context, requestID, localActor string) error {
	if localActor == "" {
		return domain.ErrNotAuthorized
	}
	if err := s.repo.SetWorkspaceRequestStatus(ctx, requestID, "rejected"); err != nil {
		return err
	}
	if s.audit != nil {
		return s.audit.Record(ctx, audit.Event{Operation: "workspace.registration.rejected", Principal: "local:" + localActor, Outcome: "success"})
	}
	return nil
}

func (s *WorkspaceService) PendingRequests(ctx context.Context) ([]domain.WorkspaceRequest, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, domain.ErrNotAuthorized
	}
	return s.repo.ListPendingWorkspaceRequests(ctx, s.now())
}

func (s *WorkspaceService) PendingRequest(ctx context.Context, id string) (domain.WorkspaceRequest, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.WorkspaceRequest{}, domain.ErrNotAuthorized
	}
	return s.repo.GetPendingWorkspaceRequest(ctx, id, s.now())
}

func (s *WorkspaceService) PendingRequestsPage(ctx context.Context, limit, offset int) ([]domain.WorkspaceRequest, *int, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, nil, domain.ErrNotAuthorized
	}
	limit, offset, err := pageBounds(limit, offset)
	if err != nil {
		return nil, nil, err
	}
	requests, hasMore, err := s.repo.ListPendingWorkspaceRequestsPage(ctx, s.now(), limit, offset)
	if err != nil {
		return nil, nil, err
	}
	return requests, pageNextOffset(offset, limit, hasMore), nil
}

func (s *WorkspaceService) PendingRequestsLocal(ctx context.Context) ([]domain.WorkspaceRequest, error) {
	return s.repo.ListPendingWorkspaceRequests(ctx, s.now())
}
