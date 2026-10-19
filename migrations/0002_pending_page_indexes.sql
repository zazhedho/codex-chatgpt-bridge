CREATE INDEX workspace_requests_pending_created
  ON workspace_requests(created_at, id) WHERE status='pending';

CREATE INDEX approvals_pending_created
  ON approvals(created_at, id) WHERE status='pending';
