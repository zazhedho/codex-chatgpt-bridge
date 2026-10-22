package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
)

const sessionAttachTTL = 15 * time.Minute
const maxCatalogPreviewRunes = 160

type sessionCatalogCursor struct {
	AppServer string `json:"a"`
	Workspace string `json:"w"`
	Search    string `json:"s"`
}

type SessionAttachRepository interface {
	SessionCatalogEnabled(context.Context) (bool, error)
	SetSessionCatalogEnabled(context.Context, bool) error
	CreateSessionAttachRequest(context.Context, domain.SessionAttachRequest) error
	GetSessionAttachRequest(context.Context, string) (domain.SessionAttachRequest, error)
	ExpireSessionAttachRequests(context.Context, time.Time) error
	ListPendingSessionAttachRequests(context.Context, time.Time) ([]domain.SessionAttachRequest, error)
	ApproveSessionAttachRequest(context.Context, string, string, domain.Thread, time.Time) error
	RejectSessionAttachRequest(context.Context, string, string, time.Time) error
	GetThread(context.Context, string) (domain.Thread, error)
	SaveThread(context.Context, domain.Thread) error
	EnsureTrustedWorkspace(context.Context, domain.Workspace) (domain.Workspace, error)
}

type SessionCatalog interface {
	ListCLISessions(context.Context, string) ([]domain.CodexSession, error)
	ListCLISessionsPage(context.Context, string, string, string, int) (domain.CodexSessionPage, error)
	ReadSessionMetadata(context.Context, string) (domain.CodexSession, error)
}

type SessionService struct {
	repo         SessionAttachRepository
	bindings     SessionProjectBindingRepository
	workspaces   *WorkspaceService
	catalog      SessionCatalog
	trustedRoots *TrustedWorkspaceRootService
	audit        *audit.Logger
	now          func() time.Time
}

func NewSessionService(repo SessionAttachRepository, workspaces *WorkspaceService, catalog SessionCatalog, auditLogger *audit.Logger, trustedRoots ...*TrustedWorkspaceRootService) *SessionService {
	var roots *TrustedWorkspaceRootService
	if len(trustedRoots) != 0 {
		roots = trustedRoots[0]
	}
	service := &SessionService{repo: repo, workspaces: workspaces, catalog: catalog, trustedRoots: roots, audit: auditLogger, now: time.Now}
	if bindings, ok := repo.(SessionProjectBindingRepository); ok {
		service.bindings = bindings
	}
	return service
}

func (s *SessionService) SetCatalogAccessLocal(ctx context.Context, enabled bool, localActor string) error {
	if localActor == "" {
		return domain.ErrNotAuthorized
	}
	if err := s.repo.SetSessionCatalogEnabled(ctx, enabled); err != nil {
		return err
	}
	if s.audit != nil {
		operation := "session.catalog-access-revoked"
		if enabled {
			operation = "session.catalog-access-granted"
		}
		if err := s.audit.Record(ctx, audit.Event{Operation: operation, Principal: "local:" + localActor, Outcome: "success"}); err != nil {
			return domain.ErrServiceUnavailable
		}
	}
	return nil
}

func (s *SessionService) CatalogAccessLocal(ctx context.Context, localActor string) (bool, error) {
	if localActor == "" {
		return false, domain.ErrNotAuthorized
	}
	return s.repo.SessionCatalogEnabled(ctx)
}

func (s *SessionService) SetWorkspaceTrustLocal(ctx context.Context, workspaceID string, trusted bool, localActor string) error {
	return s.workspaces.SetSessionTrustedLocal(ctx, workspaceID, trusted, localActor)
}

func (s *SessionService) ListCatalog(ctx context.Context, workspaceID, search, cursor string, limit int) (domain.SessionCatalogPage, error) {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.SessionCatalogPage{}, domain.ErrNotAuthorized
	}
	enabled, err := s.repo.SessionCatalogEnabled(ctx)
	if err != nil {
		return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
	}
	if !enabled {
		return domain.SessionCatalogPage{}, domain.ErrNotAuthorized
	}
	if s.catalog == nil {
		return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
	}
	if len(search) > 200 || len(cursor) > 8192 || strings.TrimSpace(search) != search {
		return domain.SessionCatalogPage{}, domain.ErrInvalidInput
	}
	for _, r := range search {
		if unicode.IsControl(r) {
			return domain.SessionCatalogPage{}, domain.ErrInvalidInput
		}
	}
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > 50 {
		return domain.SessionCatalogPage{}, domain.ErrInvalidInput
	}
	workspacePath := ""
	if workspaceID != "" {
		workspace, err := s.workspaces.Get(ctx, workspaceID)
		if err != nil {
			if s.trustedRoots == nil || !errors.Is(err, domain.ErrNotFound) || !isLogicalWorkspaceID(workspaceID) {
				return domain.SessionCatalogPage{}, err
			}
		}
		if s.trustedRoots == nil {
			workspacePath = workspace.RootPath
		}
	}
	workspaces, err := s.workspaces.List(ctx)
	if err != nil {
		return domain.SessionCatalogPage{}, err
	}
	appServerCursor := ""
	if cursor != "" {
		var decoded sessionCatalogCursor
		payload, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(payload, &decoded) != nil || decoded.AppServer == "" || decoded.Workspace != workspaceID || decoded.Search != search {
			return domain.SessionCatalogPage{}, domain.ErrInvalidInput
		}
		appServerCursor = decoded.AppServer
	}
	// Search the displayed name locally so preview-backed names remain searchable too.
	page, err := s.catalog.ListCLISessionsPage(ctx, workspacePath, "", appServerCursor, limit)
	if err != nil {
		return domain.SessionCatalogPage{}, catalogError(err)
	}
	result := domain.SessionCatalogPage{Sessions: make([]domain.SessionCatalogEntry, 0, len(page.Sessions))}
	if page.NextCursor != "" {
		payload, err := json.Marshal(sessionCatalogCursor{AppServer: page.NextCursor, Workspace: workspaceID, Search: search})
		if err != nil {
			return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
		}
		result.NextCursor = base64.RawURLEncoding.EncodeToString(payload)
	}
	for _, session := range page.Sessions {
		if !session.IsCodexCLISession() || !validThreadID(session.ID) || !filepath.IsAbs(session.WorkspacePath) || session.UpdatedAt.IsZero() {
			continue
		}
		displayName := catalogDisplayName(session)
		if search != "" && !strings.Contains(strings.ToLower(displayName), strings.ToLower(search)) {
			continue
		}
		entry := domain.SessionCatalogEntry{ThreadID: session.ID, DisplayName: displayName, Project: filepath.Base(filepath.Clean(session.WorkspacePath)), Status: session.Status, UpdatedAt: session.UpdatedAt}
		var owner *domain.Workspace
		for i := range workspaces {
			if sessionPathMatches(session.WorkspacePath, workspaces[i].RootPath) {
				owner = &workspaces[i]
				break
			}
		}
		if s.trustedRoots != nil {
			resolution, resolveErr := s.trustedRoots.ResolveSession(ctx, session.WorkspacePath)
			if resolveErr == nil {
				entry.WorkspaceID = resolution.WorkspaceID
				entry.Project = filepath.Base(resolution.ProjectPath)
				thread, err := s.repo.GetThread(ctx, session.ID)
				trustedStatus := "trusted"
				if resolution.RootScoped {
					trustedStatus = "trusted_root"
				}
				if errors.Is(err, domain.ErrNotFound) {
					entry.ConnectionStatus = trustedStatus
					entry.CanResume = resumableStatus(session.Status)
				} else if err != nil {
					return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
				} else if thread.Source == domain.ThreadSourceCodexCLIProjectBound {
					binding, workspace, bindingErr := s.projectBoundWorkspace(ctx, session, workspaceID)
					if bindingErr == nil && thread.WorkspaceID == workspace.ID && thread.SessionPath == binding.SessionPath && thread.TrustedRootID == binding.TrustedRootID {
						entry.WorkspaceID, entry.Project = workspace.ID, filepath.Base(workspace.RootPath)
						entry.ConnectionStatus, entry.CanResume = "project_bound", resumableStatus(session.Status)
					} else {
						entry.ConnectionStatus = "project_binding_revoked"
					}
				} else if thread.WorkspaceID == resolution.WorkspaceID && thread.Source == domain.ThreadSourceCodexCLIRoot && thread.TrustedRootID == resolution.Root.ID && thread.SessionPath == resolution.SessionPath {
					entry.ConnectionStatus = trustedStatus
					entry.CanResume = resumableStatus(session.Status)
				} else if owner != nil && owner.SessionTrusted && thread.WorkspaceID == owner.ID && thread.SessionPath == owner.RootPath && (thread.Source == domain.ThreadSourceCodexCLI || thread.Source == domain.ThreadSourceCodexCLITrusted) {
					entry.WorkspaceID = owner.ID
					entry.Project = owner.ID
					entry.ConnectionStatus = "trusted"
					entry.CanResume = resumableStatus(session.Status)
				} else if owner != nil && thread.WorkspaceID == owner.ID && thread.Source == domain.ThreadSourceCodexCLI && thread.SessionPath == owner.RootPath {
					entry.WorkspaceID = owner.ID
					entry.Project = owner.ID
					entry.ConnectionStatus = "attached"
					entry.CanResume = resumableStatus(session.Status)
				} else {
					entry.ConnectionStatus = "session_mapping_conflict"
				}
				if session.Status == "active" {
					entry.CanResume = false
				}
				if workspaceID != "" && entry.WorkspaceID != workspaceID {
					continue
				}
				result.Sessions = append(result.Sessions, entry)
				continue
			}
			if errors.Is(resolveErr, domain.ErrServiceUnavailable) {
				return domain.SessionCatalogPage{}, resolveErr
			}
			if errors.Is(resolveErr, domain.ErrConflict) && owner == nil {
				thread, threadErr := s.repo.GetThread(ctx, session.ID)
				if threadErr != nil && !errors.Is(threadErr, domain.ErrNotFound) {
					return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
				}
				if threadErr == nil && thread.Source == domain.ThreadSourceCodexCLIProjectBound {
					binding, workspace, bindingErr := s.projectBoundWorkspace(ctx, session, workspaceID)
					if bindingErr == nil && thread.WorkspaceID == workspace.ID && thread.SessionPath == binding.SessionPath && thread.TrustedRootID == binding.TrustedRootID {
						entry.WorkspaceID, entry.Project = workspace.ID, filepath.Base(workspace.RootPath)
						entry.ConnectionStatus, entry.CanResume = "project_bound", resumableStatus(session.Status)
					} else {
						entry.ConnectionStatus = "project_binding_revoked"
					}
				} else {
					if workspaceID != "" {
						continue
					}
					entry.ConnectionStatus = "project_selection_required"
				}
				if session.Status == "active" {
					entry.CanResume = false
				}
				result.Sessions = append(result.Sessions, entry)
				continue
			}
			if owner == nil && errors.Is(resolveErr, domain.ErrNotAuthorized) {
				continue
			}
			if !errors.Is(resolveErr, domain.ErrNotAuthorized) && !errors.Is(resolveErr, domain.ErrConflict) {
				return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
			}
			if workspaceID != "" && (owner == nil || owner.ID != workspaceID) {
				continue
			}
		}
		switch {
		case owner == nil:
			entry.ConnectionStatus = "workspace_approval_required"
		case owner.SessionTrusted:
			entry.WorkspaceID = owner.ID
			entry.Project = owner.ID
			thread, err := s.repo.GetThread(ctx, session.ID)
			if errors.Is(err, domain.ErrNotFound) {
				entry.ConnectionStatus = "trusted"
				entry.CanResume = resumableStatus(session.Status)
			} else if err != nil {
				return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
			} else if thread.WorkspaceID == owner.ID && thread.SessionPath == owner.RootPath && (thread.Source == domain.ThreadSourceCodexCLI || thread.Source == domain.ThreadSourceCodexCLITrusted) {
				entry.ConnectionStatus = "trusted"
				entry.CanResume = resumableStatus(session.Status)
			} else {
				entry.ConnectionStatus = "session_mapping_conflict"
			}
		default:
			entry.WorkspaceID = owner.ID
			entry.Project = owner.ID
			thread, err := s.repo.GetThread(ctx, session.ID)
			if err == nil && thread.WorkspaceID == owner.ID && thread.Source == domain.ThreadSourceCodexCLI && thread.SessionPath == owner.RootPath {
				entry.ConnectionStatus = "attached"
				entry.CanResume = resumableStatus(session.Status)
			} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return domain.SessionCatalogPage{}, domain.ErrServiceUnavailable
			} else {
				entry.ConnectionStatus = "attach_approval_required"
			}
		}
		if session.Status == "active" {
			entry.CanResume = false
		}
		result.Sessions = append(result.Sessions, entry)
	}
	return result, nil
}

func catalogDisplayName(session domain.CodexSession) string {
	if name := sanitizeCatalogText(session.Title, 0); name != "" {
		return name
	}
	if preview := sanitizeCatalogText(session.Preview, maxCatalogPreviewRunes); preview != "" {
		return preview
	}
	return "Untitled session"
}

func sanitizeCatalogText(value string, maxRunes int) string {
	value = security.RedactSecrets(value)
	var clean strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			clean.WriteByte(' ')
		} else {
			clean.WriteRune(r)
		}
	}
	value = strings.Join(strings.Fields(clean.String()), " ")
	if maxRunes > 0 {
		runes := []rune(value)
		if len(runes) > maxRunes {
			value = string(runes[:maxRunes-1]) + "…"
		}
	}
	return value
}

// AuthorizeResume binds one explicitly selected session to its authorized workspace.
func (s *SessionService) AuthorizeResume(ctx context.Context, workspaceID, threadID string) error {
	if _, ok := security.Authorize(ctx); !ok {
		return domain.ErrNotAuthorized
	}
	if !validThreadID(threadID) || s.catalog == nil {
		return domain.ErrInvalidInput
	}
	thread, threadErr := s.repo.GetThread(ctx, threadID)
	if threadErr != nil && !errors.Is(threadErr, domain.ErrNotFound) {
		return threadErr
	}
	session, err := s.catalog.ReadSessionMetadata(ctx, threadID)
	if err != nil {
		return catalogError(err)
	}
	if session.ID != threadID {
		return domain.ErrNotAuthorized
	}
	if !resumableStatus(session.Status) {
		return domain.ErrConflict
	}
	if threadErr == nil && thread.Source == domain.ThreadSourceCodexCLIProjectBound {
		if err := s.authorizeProjectBoundThread(ctx, workspaceID, thread, session); err != nil {
			return err
		}
		return nil
	}
	if threadErr == nil && thread.Source == domain.ThreadSourceCodexCLIRoot {
		return s.authorizeRootThread(ctx, workspaceID, thread, session)
	}
	if errors.Is(threadErr, domain.ErrNotFound) && s.trustedRoots != nil && session.IsCodexCLISession() {
		resolution, rootErr := s.trustedRoots.ResolveSession(ctx, session.WorkspacePath)
		if rootErr == nil {
			if resolution.WorkspaceID != workspaceID {
				return domain.ErrNotAuthorized
			}
			workspace, err := s.trustedRoots.EnsureWorkspace(ctx, resolution)
			if err != nil {
				return err
			}
			if !sessionPathWithinWorkspace(session.WorkspacePath, workspace.RootPath) {
				return domain.ErrNotAuthorized
			}
			thread = domain.Thread{ID: session.ID, WorkspaceID: workspace.ID, Source: domain.ThreadSourceCodexCLIRoot, SessionPath: resolution.SessionPath, TrustedRootID: resolution.Root.ID, CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt}
			if err := s.repo.SaveThread(ctx, thread); err != nil {
				return err
			}
			if s.audit != nil {
				if err := s.audit.Record(ctx, audit.Event{Operation: "session.trusted-root-session-selected", Principal: "mcp-session", WorkspaceID: workspace.ID, ResourceID: thread.ID, Outcome: "success"}); err != nil {
					return domain.ErrServiceUnavailable
				}
			}
			return nil
		}
		if errors.Is(rootErr, domain.ErrServiceUnavailable) {
			return rootErr
		}
		if errors.Is(rootErr, domain.ErrConflict) {
			return domain.ErrNotAuthorized
		}
	}
	workspace, err := s.workspaces.Get(ctx, workspaceID)
	if err != nil {
		return err
	}
	if errors.Is(threadErr, domain.ErrNotFound) {
		if !workspace.SessionTrusted || !sessionCanAttach(session, workspace) {
			return domain.ErrNotAuthorized
		}
		bound := domain.Thread{ID: session.ID, WorkspaceID: workspace.ID, Source: domain.ThreadSourceCodexCLITrusted, SessionPath: workspace.RootPath, CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt}
		if err := s.repo.SaveThread(ctx, bound); err != nil {
			return err
		}
		if s.audit != nil {
			if err := s.audit.Record(ctx, audit.Event{Operation: "session.trusted-session-selected", Principal: "mcp-session", WorkspaceID: workspace.ID, ResourceID: bound.ID, Outcome: "success"}); err != nil {
				return domain.ErrServiceUnavailable
			}
		}
		return nil
	}
	if thread.WorkspaceID != workspace.ID || !sessionPathMatches(session.WorkspacePath, workspace.RootPath) {
		return domain.ErrNotAuthorized
	}
	switch thread.Source {
	case domain.ThreadSourceBridge:
		return nil
	case domain.ThreadSourceCodexCLITrusted:
		if workspace.SessionTrusted && thread.SessionPath == workspace.RootPath {
			return nil
		}
	case domain.ThreadSourceCodexCLI:
		if thread.SessionPath == workspace.RootPath && sessionCanAttach(session, workspace) {
			return nil
		}
	}
	return domain.ErrNotAuthorized
}

func (s *SessionService) authorizeRootThread(ctx context.Context, workspaceID string, thread domain.Thread, session domain.CodexSession) error {
	if s.trustedRoots == nil || !session.IsCodexCLISession() || thread.TrustedRootID == "" {
		return domain.ErrNotAuthorized
	}
	resolution, err := s.trustedRoots.ResolveSession(ctx, session.WorkspacePath)
	if err != nil || resolution.Root.ID != thread.TrustedRootID || resolution.WorkspaceID != workspaceID || thread.WorkspaceID != workspaceID || thread.SessionPath != resolution.SessionPath {
		return domain.ErrNotAuthorized
	}
	workspace, err := s.trustedRoots.EnsureWorkspace(ctx, resolution)
	if err != nil {
		return err
	}
	if workspace.ID != workspaceID || !sessionPathWithinWorkspace(session.WorkspacePath, workspace.RootPath) {
		return domain.ErrNotAuthorized
	}
	return nil
}

func sessionPathWithinWorkspace(sessionPath, workspacePath string) bool {
	canonical, err := canonicalSessionDirectory(sessionPath)
	return err == nil && security.PathWithin(workspacePath, canonical)
}

func resumableStatus(status string) bool {
	return status == "idle" || status == "notLoaded"
}

func (s *SessionService) ListLocal(ctx context.Context, workspaceID, localActor string) ([]domain.CodexSession, error) {
	if localActor == "" || s.catalog == nil {
		return nil, domain.ErrNotAuthorized
	}
	workspace, err := s.workspaces.GetLocal(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	sessions, err := s.catalog.ListCLISessions(ctx, workspace.RootPath)
	if err != nil {
		return nil, domain.ErrServiceUnavailable
	}
	result := make([]domain.CodexSession, 0, len(sessions))
	for _, session := range sessions {
		if !session.IsCodexCLISession() || session.ID == "" || !sessionPathMatches(session.WorkspacePath, workspace.RootPath) {
			continue
		}
		result = append(result, session)
	}
	return result, nil
}

func (s *SessionService) RequestAttach(ctx context.Context, workspaceID, threadID string) (domain.SessionAttachRequest, error) {
	principal, ok := security.Authorize(ctx)
	if !ok {
		return domain.SessionAttachRequest{}, domain.ErrNotAuthorized
	}
	if !validThreadID(threadID) || s.catalog == nil {
		return domain.SessionAttachRequest{}, domain.ErrInvalidInput
	}
	workspace, err := s.workspaces.Get(ctx, workspaceID)
	if err != nil {
		return domain.SessionAttachRequest{}, err
	}
	if _, err := s.repo.GetThread(ctx, threadID); err == nil {
		return domain.SessionAttachRequest{}, domain.ErrConflict
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.SessionAttachRequest{}, err
	}
	available, err := s.catalog.ListCLISessions(ctx, workspace.RootPath)
	if err != nil {
		return domain.SessionAttachRequest{}, catalogError(err)
	}
	var session domain.CodexSession
	for _, candidate := range available {
		if candidate.ID == threadID {
			session = candidate
			break
		}
	}
	if session.ID != threadID || !sessionCanAttach(session, workspace) {
		return domain.SessionAttachRequest{}, domain.ErrNotAuthorized
	}
	id, err := newID()
	if err != nil {
		return domain.SessionAttachRequest{}, err
	}
	now := s.now().UTC()
	request := domain.SessionAttachRequest{
		ID: id, ThreadID: session.ID, WorkspaceID: workspace.ID, WorkspacePath: workspace.RootPath,
		RootDevice: workspace.RootDevice, RootInode: workspace.RootInode, SessionUpdatedAt: session.UpdatedAt,
		Status: domain.SessionAttachPending, CreatedAt: now, ExpiresAt: now.Add(sessionAttachTTL),
	}
	if err := s.repo.CreateSessionAttachRequest(ctx, request); err != nil {
		return domain.SessionAttachRequest{}, err
	}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "session.attach.requested", Principal: principal, WorkspaceID: workspace.ID, ResourceID: session.ID, Outcome: "pending"}); err != nil {
			return domain.SessionAttachRequest{}, domain.ErrServiceUnavailable
		}
	}
	return request, nil
}

func (s *SessionService) PendingLocal(ctx context.Context, localActor string) ([]domain.SessionAttachRequest, error) {
	if localActor == "" {
		return nil, domain.ErrNotAuthorized
	}
	now := s.now().UTC()
	if err := s.repo.ExpireSessionAttachRequests(ctx, now); err != nil {
		return nil, err
	}
	return s.repo.ListPendingSessionAttachRequests(ctx, now)
}

func (s *SessionService) ShowPendingLocal(ctx context.Context, requestID, localActor string) (domain.SessionAttachDetails, error) {
	if localActor == "" || s.catalog == nil {
		return domain.SessionAttachDetails{}, domain.ErrNotAuthorized
	}
	request, err := s.repo.GetSessionAttachRequest(ctx, requestID)
	if err != nil {
		return domain.SessionAttachDetails{}, err
	}
	if request.Status != domain.SessionAttachPending || !s.now().Before(request.ExpiresAt) {
		return domain.SessionAttachDetails{}, domain.ErrConflict
	}
	workspace, err := s.workspaces.GetLocal(ctx, request.WorkspaceID)
	if err != nil || !sameWorkspaceIdentity(workspace, request) {
		return domain.SessionAttachDetails{}, domain.ErrNotAuthorized
	}
	session, err := s.catalog.ReadSessionMetadata(ctx, request.ThreadID)
	if err != nil {
		return domain.SessionAttachDetails{}, catalogError(err)
	}
	if !sessionPathMatches(session.WorkspacePath, workspace.RootPath) || !session.IsCodexCLISession() {
		return domain.SessionAttachDetails{}, domain.ErrNotAuthorized
	}
	return domain.SessionAttachDetails{Request: request, Session: session}, nil
}

func (s *SessionService) ApproveLocal(ctx context.Context, requestID, localActor string) (domain.Thread, error) {
	if localActor == "" || s.catalog == nil {
		return domain.Thread{}, domain.ErrNotAuthorized
	}
	request, err := s.repo.GetSessionAttachRequest(ctx, requestID)
	if err != nil {
		return domain.Thread{}, err
	}
	now := s.now().UTC()
	if request.Status != domain.SessionAttachPending || !now.Before(request.ExpiresAt) {
		if request.Status == domain.SessionAttachPending {
			_ = s.repo.ExpireSessionAttachRequests(ctx, now)
		}
		return domain.Thread{}, domain.ErrConflict
	}
	workspace, err := s.workspaces.GetLocal(ctx, request.WorkspaceID)
	if err != nil || !sameWorkspaceIdentity(workspace, request) {
		return domain.Thread{}, domain.ErrNotAuthorized
	}
	session, err := s.catalog.ReadSessionMetadata(ctx, request.ThreadID)
	if err != nil {
		return domain.Thread{}, catalogError(err)
	}
	if session.ID != request.ThreadID || !sessionCanAttach(session, workspace) {
		return domain.Thread{}, domain.ErrNotAuthorized
	}
	if !session.UpdatedAt.Equal(request.SessionUpdatedAt) {
		return domain.Thread{}, domain.ErrConflict
	}
	thread := domain.Thread{ID: session.ID, WorkspaceID: workspace.ID, Source: domain.ThreadSourceCodexCLI, SessionPath: workspace.RootPath, CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt}
	if err := s.repo.ApproveSessionAttachRequest(ctx, requestID, localActor, thread, now); err != nil {
		return domain.Thread{}, err
	}
	if s.audit != nil {
		if err := s.audit.Record(ctx, audit.Event{Operation: "session.attach.approved", Principal: "local:" + localActor, WorkspaceID: workspace.ID, ResourceID: thread.ID, Outcome: "success"}); err != nil {
			return thread, domain.ErrServiceUnavailable
		}
	}
	return thread, nil
}

func (s *SessionService) RejectLocal(ctx context.Context, requestID, localActor string) error {
	if localActor == "" {
		return domain.ErrNotAuthorized
	}
	request, err := s.repo.GetSessionAttachRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if err := s.repo.RejectSessionAttachRequest(ctx, requestID, localActor, s.now().UTC()); err != nil {
		return err
	}
	if s.audit != nil {
		return s.audit.Record(ctx, audit.Event{Operation: "session.attach.rejected", Principal: "local:" + localActor, WorkspaceID: request.WorkspaceID, ResourceID: request.ThreadID, Outcome: "success"})
	}
	return nil
}

func sessionCanAttach(session domain.CodexSession, workspace domain.Workspace) bool {
	return session.ID != "" && session.IsCodexCLISession() && (session.Status == "idle" || session.Status == "notLoaded") &&
		!session.CreatedAt.IsZero() && !session.UpdatedAt.IsZero() && sessionPathMatches(session.WorkspacePath, workspace.RootPath)
}

func sessionPathMatches(sessionPath, workspacePath string) bool {
	if !filepath.IsAbs(sessionPath) || !filepath.IsAbs(workspacePath) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(sessionPath)
	return err == nil && filepath.Clean(resolved) == filepath.Clean(workspacePath)
}

func sameWorkspaceIdentity(workspace domain.Workspace, request domain.SessionAttachRequest) bool {
	return workspace.ID == request.WorkspaceID && workspace.RootPath == request.WorkspacePath &&
		workspace.RootDevice == request.RootDevice && workspace.RootInode == request.RootInode
}

func validThreadID(threadID string) bool {
	if threadID == "" || len(threadID) > 256 || strings.TrimSpace(threadID) != threadID {
		return false
	}
	for _, r := range threadID {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func isLogicalWorkspaceID(id string) bool {
	if len(id) != len("root-")+24 || !strings.HasPrefix(id, "root-") {
		return false
	}
	for _, r := range id[len("root-"):] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func catalogError(err error) error {
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalidInput) {
		return err
	}
	return domain.ErrServiceUnavailable
}
