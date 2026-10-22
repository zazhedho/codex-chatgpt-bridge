package service

import (
	"context"
	"errors"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
)

const sessionProjectBindingTTL = 15 * time.Minute

type SessionProjectBindingRepository interface {
	CreateSessionProjectBinding(context.Context, domain.SessionProjectBinding) error
	GetSessionProjectBinding(context.Context, string) (domain.SessionProjectBinding, error)
	GetApprovedSessionProjectBinding(context.Context, string) (domain.SessionProjectBinding, error)
	ListPendingSessionProjectBindings(context.Context, time.Time) ([]domain.SessionProjectBinding, error)
	ListApprovedSessionProjectBindings(context.Context) ([]domain.SessionProjectBinding, error)
	ApproveSessionProjectBinding(context.Context, string, string, domain.Workspace, domain.Thread, time.Time) (domain.Workspace, error)
	RejectSessionProjectBinding(context.Context, string, string, time.Time) error
	RevokeSessionProjectBinding(context.Context, string, string, time.Time) error
}

func (s *SessionService) ListProjectChoices(ctx context.Context, threadID string, offset, limit int) ([]domain.SessionProjectChoice, *int, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return nil, nil, domain.ErrNotAuthorized
	}
	if !validThreadID(threadID) || s.catalog == nil || s.trustedRoots == nil || offset < 0 || limit < 1 || limit > 50 {
		return nil, nil, domain.ErrInvalidInput
	}
	enabled, err := s.repo.SessionCatalogEnabled(ctx)
	if err != nil || !enabled {
		if err != nil {
			return nil, nil, domain.ErrServiceUnavailable
		}
		return nil, nil, domain.ErrNotAuthorized
	}
	session, err := s.catalog.ReadSessionMetadata(ctx, threadID)
	if err != nil {
		return nil, nil, catalogError(err)
	}
	if session.ID != threadID || !session.IsCodexCLISession() || !resumableStatus(session.Status) {
		return nil, nil, domain.ErrConflict
	}
	if _, err := s.repo.GetThread(ctx, threadID); err == nil {
		return nil, nil, domain.ErrConflict
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, nil, domain.ErrServiceUnavailable
	}
	return s.trustedRoots.ProjectChoices(ctx, session.WorkspacePath, offset, limit)
}

func (s *SessionService) RequestProjectBinding(ctx context.Context, threadID, projectID string) (domain.SessionProjectBinding, error) {
	principal, ok := security.Authorize(ctx)
	if !ok {
		return domain.SessionProjectBinding{}, domain.ErrNotAuthorized
	}
	if !validThreadID(threadID) || len(projectID) != 32 || s.catalog == nil || s.trustedRoots == nil || s.bindings == nil {
		return domain.SessionProjectBinding{}, domain.ErrInvalidInput
	}
	enabled, err := s.repo.SessionCatalogEnabled(ctx)
	if err != nil || !enabled {
		if err != nil {
			return domain.SessionProjectBinding{}, domain.ErrServiceUnavailable
		}
		return domain.SessionProjectBinding{}, domain.ErrNotAuthorized
	}
	if _, err := s.repo.GetThread(ctx, threadID); err == nil {
		return domain.SessionProjectBinding{}, domain.ErrConflict
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.SessionProjectBinding{}, domain.ErrServiceUnavailable
	}
	session, err := s.catalog.ReadSessionMetadata(ctx, threadID)
	if err != nil {
		return domain.SessionProjectBinding{}, catalogError(err)
	}
	if session.ID != threadID || !session.IsCodexCLISession() || !resumableStatus(session.Status) || session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() {
		return domain.SessionProjectBinding{}, domain.ErrConflict
	}
	resolution, err := s.trustedRoots.resolveProjectChoice(ctx, session.WorkspacePath, projectID)
	if err != nil {
		return domain.SessionProjectBinding{}, err
	}
	id, err := newID()
	if err != nil {
		return domain.SessionProjectBinding{}, err
	}
	now := s.now().UTC()
	binding := domain.SessionProjectBinding{
		ID: id, ThreadID: threadID, ProjectID: projectID, TrustedRootID: resolution.Root.ID, TrustedRootPath: resolution.Root.RootPath,
		RootDevice: resolution.Root.RootDevice, RootInode: resolution.Root.RootInode, SessionPath: resolution.SessionPath,
		SessionDevice: resolution.SessionDevice, SessionInode: resolution.SessionInode, SessionCreatedAt: session.CreatedAt,
		SessionUpdatedAt: session.UpdatedAt, ProjectPath: resolution.ProjectPath, ProjectDevice: resolution.ProjectDevice,
		ProjectInode: resolution.ProjectInode, Status: domain.SessionProjectBindingPending, RequestedBy: principal,
		CreatedAt: now, ExpiresAt: now.Add(sessionProjectBindingTTL),
	}
	if err := s.bindings.CreateSessionProjectBinding(ctx, binding); err != nil {
		return domain.SessionProjectBinding{}, err
	}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "session.project-binding.requested", Principal: principal, ResourceID: threadID, Outcome: "pending"}); err != nil {
			return binding, domain.ErrServiceUnavailable
		}
	}
	return binding, nil
}

func (s *SessionService) PendingProjectBindingsLocal(ctx context.Context, actor string) ([]domain.SessionProjectBinding, error) {
	if actor == "" || s.bindings == nil {
		return nil, domain.ErrNotAuthorized
	}
	return s.bindings.ListPendingSessionProjectBindings(ctx, s.now().UTC())
}

func (s *SessionService) ProjectBindingLocal(ctx context.Context, id, actor string) (domain.SessionProjectBinding, error) {
	if actor == "" || s.bindings == nil {
		return domain.SessionProjectBinding{}, domain.ErrNotAuthorized
	}
	return s.bindings.GetSessionProjectBinding(ctx, id)
}

func (s *SessionService) ListProjectBindingsLocal(ctx context.Context, actor string) ([]domain.SessionProjectBinding, error) {
	if actor == "" || s.bindings == nil {
		return nil, domain.ErrNotAuthorized
	}
	return s.bindings.ListApprovedSessionProjectBindings(ctx)
}

func (s *SessionService) ApproveProjectBindingLocal(ctx context.Context, id, actor string) (domain.SessionProjectBinding, error) {
	if actor == "" || s.bindings == nil || s.catalog == nil || s.trustedRoots == nil {
		return domain.SessionProjectBinding{}, domain.ErrNotAuthorized
	}
	binding, err := s.bindings.GetSessionProjectBinding(ctx, id)
	if err != nil {
		return domain.SessionProjectBinding{}, err
	}
	now := s.now().UTC()
	if binding.Status != domain.SessionProjectBindingPending || !now.Before(binding.ExpiresAt) {
		return domain.SessionProjectBinding{}, domain.ErrConflict
	}
	session, err := s.catalog.ReadSessionMetadata(ctx, binding.ThreadID)
	if err != nil {
		return domain.SessionProjectBinding{}, catalogError(err)
	}
	sessionPath, pathErr := canonicalSessionDirectory(session.WorkspacePath)
	if session.ID != binding.ThreadID || !session.IsCodexCLISession() || !resumableStatus(session.Status) ||
		pathErr != nil || sessionPath != binding.SessionPath || !session.CreatedAt.Equal(binding.SessionCreatedAt) || !session.UpdatedAt.Equal(binding.SessionUpdatedAt) {
		return domain.SessionProjectBinding{}, domain.ErrConflict
	}
	resolution, err := s.trustedRoots.resolveProjectChoice(ctx, binding.SessionPath, binding.ProjectID)
	if err != nil || resolution.Root.ID != binding.TrustedRootID || resolution.Root.RootPath != binding.TrustedRootPath ||
		resolution.Root.RootDevice != binding.RootDevice || resolution.Root.RootInode != binding.RootInode ||
		resolution.SessionDevice != binding.SessionDevice || resolution.SessionInode != binding.SessionInode ||
		resolution.ProjectPath != binding.ProjectPath || resolution.ProjectDevice != binding.ProjectDevice || resolution.ProjectInode != binding.ProjectInode {
		return domain.SessionProjectBinding{}, domain.ErrNotAuthorized
	}
	workspace := domain.Workspace{ID: resolution.WorkspaceID, RootPath: resolution.ProjectPath, RootDevice: resolution.ProjectDevice, RootInode: resolution.ProjectInode, CreatedAt: now, TrustedRootID: binding.TrustedRootID, RootSessionOnly: true}
	thread := domain.Thread{ID: session.ID, WorkspaceID: workspace.ID, Source: domain.ThreadSourceCodexCLIProjectBound, SessionPath: binding.SessionPath, TrustedRootID: binding.TrustedRootID, CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt}
	workspace, err = s.bindings.ApproveSessionProjectBinding(ctx, id, actor, workspace, thread, now)
	if err != nil {
		return domain.SessionProjectBinding{}, err
	}
	binding.Status, binding.WorkspaceID, binding.ApprovedBy, binding.DecidedAt = domain.SessionProjectBindingApproved, workspace.ID, actor, now
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "session.project-binding.approved", Principal: "local:" + actor, WorkspaceID: workspace.ID, ResourceID: session.ID, Outcome: "success"}); err != nil {
			return binding, domain.ErrServiceUnavailable
		}
	}
	return binding, nil
}

func (s *SessionService) RejectProjectBindingLocal(ctx context.Context, id, actor string) error {
	if actor == "" || s.bindings == nil {
		return domain.ErrNotAuthorized
	}
	if err := s.bindings.RejectSessionProjectBinding(ctx, id, actor, s.now().UTC()); err != nil {
		return err
	}
	if s.audit != nil {
		return s.audit.Record(ctx, audit.Event{Operation: "session.project-binding.rejected", Principal: "local:" + actor, ResourceID: id, Outcome: "success"})
	}
	return nil
}

func (s *SessionService) RevokeProjectBindingLocal(ctx context.Context, threadID, actor string) error {
	if actor == "" || s.bindings == nil {
		return domain.ErrNotAuthorized
	}
	binding, err := s.bindings.GetApprovedSessionProjectBinding(ctx, threadID)
	if err != nil {
		return err
	}
	if err := s.bindings.RevokeSessionProjectBinding(ctx, threadID, actor, s.now().UTC()); err != nil {
		return err
	}
	if s.audit != nil {
		return s.audit.Record(ctx, audit.Event{Operation: "session.project-binding.revoked", Principal: "local:" + actor, WorkspaceID: binding.WorkspaceID, ResourceID: threadID, Outcome: "success"})
	}
	return nil
}

func (s *SessionService) projectBoundWorkspace(ctx context.Context, session domain.CodexSession, workspaceID string) (domain.SessionProjectBinding, domain.Workspace, error) {
	if s.bindings == nil || s.trustedRoots == nil || !session.IsCodexCLISession() {
		return domain.SessionProjectBinding{}, domain.Workspace{}, domain.ErrNotAuthorized
	}
	binding, err := s.bindings.GetApprovedSessionProjectBinding(ctx, session.ID)
	if err != nil {
		return domain.SessionProjectBinding{}, domain.Workspace{}, err
	}
	if workspaceID != "" && binding.WorkspaceID != workspaceID {
		return domain.SessionProjectBinding{}, domain.Workspace{}, domain.ErrNotAuthorized
	}
	workspace, err := s.workspaces.Get(ctx, binding.WorkspaceID)
	if err != nil {
		return domain.SessionProjectBinding{}, domain.Workspace{}, domain.ErrNotAuthorized
	}
	if s.trustedRoots.ValidateProjectBinding(ctx, binding, session, workspace) != nil {
		return domain.SessionProjectBinding{}, domain.Workspace{}, domain.ErrNotAuthorized
	}
	return binding, workspace, nil
}

func (s *SessionService) authorizeProjectBoundThread(ctx context.Context, workspaceID string, thread domain.Thread, session domain.CodexSession) error {
	binding, workspace, err := s.projectBoundWorkspace(ctx, session, workspaceID)
	if err != nil {
		return domain.ErrNotAuthorized
	}
	if thread.WorkspaceID != workspace.ID || thread.SessionPath != binding.SessionPath || thread.TrustedRootID != binding.TrustedRootID {
		return domain.ErrNotAuthorized
	}
	return nil
}

func (s *SessionService) ValidateThread(ctx context.Context, thread domain.Thread, workspace domain.Workspace) error {
	if s.catalog == nil || s.trustedRoots == nil {
		return domain.ErrNotAuthorized
	}
	session, err := s.catalog.ReadSessionMetadata(ctx, thread.ID)
	if err != nil || !resumableStatus(session.Status) {
		return domain.ErrNotAuthorized
	}
	switch thread.Source {
	case domain.ThreadSourceCodexCLIRoot:
		return s.trustedRoots.ValidateThread(ctx, thread, workspace)
	case domain.ThreadSourceCodexCLIProjectBound:
		if s.bindings == nil {
			return domain.ErrNotAuthorized
		}
		binding, err := s.bindings.GetApprovedSessionProjectBinding(ctx, thread.ID)
		if err != nil || thread.WorkspaceID != workspace.ID || binding.WorkspaceID != workspace.ID || thread.SessionPath != binding.SessionPath || thread.TrustedRootID != binding.TrustedRootID {
			return domain.ErrNotAuthorized
		}
		return s.trustedRoots.ValidateProjectBinding(ctx, binding, session, workspace)
	default:
		return domain.ErrNotAuthorized
	}
}
