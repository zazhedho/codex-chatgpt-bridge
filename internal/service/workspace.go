package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"example.com/codex-chatgpt-bridge/internal/audit"
	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/security"
)

var workspaceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

type WorkspaceRepository interface {
	ListWorkspaces(context.Context) ([]domain.Workspace, error)
	GetWorkspace(context.Context, string) (domain.Workspace, error)
	InsertWorkspaceRequest(context.Context, domain.WorkspaceRequest) error
	ListPendingWorkspaceRequests(context.Context, time.Time) ([]domain.WorkspaceRequest, error)
	GetWorkspaceRequest(context.Context, string) (domain.WorkspaceRequest, error)
	ApproveWorkspaceRequest(context.Context, string, domain.Workspace) error
	SetWorkspaceRequestStatus(context.Context, string, string) error
}

type WorkspaceService struct {
	repo      WorkspaceRepository
	audit     *audit.Logger
	home      string
	codexHome string
	now       func() time.Time
}

func NewWorkspaceService(repo WorkspaceRepository, auditLogger *audit.Logger, home, codexHome string) *WorkspaceService {
	return &WorkspaceService{repo: repo, audit: auditLogger, home: home, codexHome: codexHome, now: time.Now}
}

func (s *WorkspaceService) List(ctx context.Context) ([]domain.Workspace, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, domain.ErrNotAuthorized
	}
	return s.list(ctx)
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
		if err := security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: workspace.RootPath, Device: workspace.RootDevice, Inode: workspace.RootInode}, s.home, s.codexHome); err != nil {
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
	if !workspaceIDPattern.MatchString(id) {
		return domain.Workspace{}, domain.ErrInvalidInput
	}
	workspace, err := s.repo.GetWorkspace(ctx, id)
	if err != nil {
		return domain.Workspace{}, err
	}
	if err := security.RevalidateWorkspaceRoot(security.WorkspaceRoot{Path: workspace.RootPath, Device: workspace.RootDevice, Inode: workspace.RootInode}, s.home, s.codexHome); err != nil {
		return domain.Workspace{}, domain.ErrNotAuthorized
	}
	return workspace, nil
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
	workspaces, err := s.repo.ListWorkspaces(ctx)
	if err != nil {
		return domain.WorkspaceRequest{}, err
	}
	requests, err := s.repo.ListPendingWorkspaceRequests(ctx, s.now())
	if err != nil {
		return domain.WorkspaceRequest{}, err
	}
	for _, workspace := range workspaces {
		if workspace.ID == id || workspace.RootPath == root.Path {
			return domain.WorkspaceRequest{}, domain.ErrConflict
		}
	}
	for _, request := range requests {
		if request.WorkspaceID == id || request.RootPath == root.Path {
			return domain.WorkspaceRequest{}, domain.ErrConflict
		}
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

func (s *WorkspaceService) PendingRequestsLocal(ctx context.Context) ([]domain.WorkspaceRequest, error) {
	return s.repo.ListPendingWorkspaceRequests(ctx, s.now())
}
