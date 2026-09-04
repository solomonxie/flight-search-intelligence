package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AgentRequestRow is one agent_requests row — the agent loop's durable
// state (see DESIGN.md "Agent loop"). spec_json/rounds_json are opaque
// JSON blobs here; internal/agents owns their shape, keeping this package
// free of a dependency on it.
type AgentRequestRow struct {
	RequestID     string
	SpecJSON      string
	RoundsJSON    string
	Status        string
	DeferredUntil sql.NullString
	EmailBody     sql.NullString
	FinalizedBy   sql.NullString
	CreatedAt     string
	UpdatedAt     string
}

// AgentTaskRow is one agent_tasks row — the unit cmd/collector -worker
// claims and runs.
type AgentTaskRow struct {
	TaskID     string
	RequestID  string
	Round      int
	ParamsJSON string
	Status     string
	ClaimedBy  string
	ClaimedAt  sql.NullString
	Attempt    int
	ResultJSON sql.NullString
	Error      sql.NullString
	CreatedAt  string
	UpdatedAt  string
}

// CreateAgentRequest inserts a new request row in status
// "awaiting_decision" with an empty round history — the entry point for
// both cmd/email-intake -start and a real (future) SES-triggered create.
func (s *SQLite) CreateAgentRequest(ctx context.Context, requestID string, specJSON []byte) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_requests (request_id, spec_json, rounds_json, status, created_at, updated_at)
		VALUES (?, ?, '[]', 'awaiting_decision', ?, ?)`,
		requestID, string(specJSON), now, now)
	if err != nil {
		return fmt.Errorf("catalog: creating agent request: %w", err)
	}
	return nil
}

// LoadAgentRequest reads one request row by id.
func (s *SQLite) LoadAgentRequest(ctx context.Context, requestID string) (AgentRequestRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT request_id, spec_json, rounds_json, status, deferred_until, email_body, finalized_by, created_at, updated_at
		FROM agent_requests WHERE request_id = ?`, requestID)
	var r AgentRequestRow
	err := row.Scan(&r.RequestID, &r.SpecJSON, &r.RoundsJSON, &r.Status,
		&r.DeferredUntil, &r.EmailBody, &r.FinalizedBy, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return AgentRequestRow{}, fmt.Errorf("catalog: loading agent request %s: %w", requestID, err)
	}
	return r, nil
}

// ListActiveAgentRequests returns every request not yet finalized — what
// a reconciler poll tick (cmd/email-intake -worker) iterates over.
func (s *SQLite) ListActiveAgentRequests(ctx context.Context) ([]AgentRequestRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT request_id, spec_json, rounds_json, status, deferred_until, email_body, finalized_by, created_at, updated_at
		FROM agent_requests WHERE status != 'finalized'`)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing active agent requests: %w", err)
	}
	defer rows.Close()

	var out []AgentRequestRow
	for rows.Next() {
		var r AgentRequestRow
		if err := rows.Scan(&r.RequestID, &r.SpecJSON, &r.RoundsJSON, &r.Status,
			&r.DeferredUntil, &r.EmailBody, &r.FinalizedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("catalog: scanning agent request: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateAgentRequestSpec overwrites spec_json only — how a follow-up
// signal (cmd/email-intake -signal) lands a new soft constraint without
// disturbing whatever round is currently in flight.
func (s *SQLite) UpdateAgentRequestSpec(ctx context.Context, requestID string, specJSON []byte) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_requests SET spec_json = ?, updated_at = ? WHERE request_id = ?`,
		string(specJSON), now, requestID)
	if err != nil {
		return fmt.Errorf("catalog: updating agent request spec: %w", err)
	}
	return mustAffectOne(res, "agent request", requestID)
}

// SaveAgentRequestState is the reconciler's main state-transition write:
// new status, new round history, and (only meaningful once status is
// "finalized") the drafted email and why it stopped.
func (s *SQLite) SaveAgentRequestState(ctx context.Context, requestID, status string, roundsJSON []byte, deferredUntil *time.Time, emailBody, finalizedBy string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var deferredStr sql.NullString
	if deferredUntil != nil {
		deferredStr = sql.NullString{String: deferredUntil.UTC().Format(time.RFC3339), Valid: true}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_requests
		SET status = ?, rounds_json = ?, deferred_until = ?, email_body = ?, finalized_by = ?, updated_at = ?
		WHERE request_id = ?`,
		status, string(roundsJSON), deferredStr, nullIfEmpty(emailBody), nullIfEmpty(finalizedBy), now, requestID)
	if err != nil {
		return fmt.Errorf("catalog: saving agent request state: %w", err)
	}
	return mustAffectOne(res, "agent request", requestID)
}

// CreateAgentTask inserts a new pending task — one dispatched search.
func (s *SQLite) CreateAgentTask(ctx context.Context, taskID, requestID string, round int, paramsJSON []byte) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_tasks (task_id, request_id, round, params_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?)`,
		taskID, requestID, round, string(paramsJSON), now, now)
	if err != nil {
		return fmt.Errorf("catalog: creating agent task: %w", err)
	}
	return nil
}

// GetAgentTask reads one task row by id — how the reconciler checks
// whether the round it's waiting on has finished.
func (s *SQLite) GetAgentTask(ctx context.Context, taskID string) (AgentTaskRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT task_id, request_id, round, params_json, status, claimed_by, claimed_at, attempt, result_json, error, created_at, updated_at
		FROM agent_tasks WHERE task_id = ?`, taskID)
	var t AgentTaskRow
	err := row.Scan(&t.TaskID, &t.RequestID, &t.Round, &t.ParamsJSON, &t.Status,
		&t.ClaimedBy, &t.ClaimedAt, &t.Attempt, &t.ResultJSON, &t.Error, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return AgentTaskRow{}, fmt.Errorf("catalog: loading agent task %s: %w", taskID, err)
	}
	return t, nil
}

// ClaimPendingAgentTask atomically claims one pending task for workerID —
// the lease that stands in for a Temporal Activity's own dispatch. Returns
// ok=false when there's nothing to claim right now, not an error.
// SQLite has no "UPDATE ... LIMIT 1 RETURNING" it can rely on across
// concurrent callers, so this selects a candidate then re-checks the
// UPDATE's affected-row count to detect (and just skip, not error on) a
// lost race against another claimer.
func (s *SQLite) ClaimPendingAgentTask(ctx context.Context, workerID string) (AgentTaskRow, bool, error) {
	var taskID string
	err := s.db.QueryRowContext(ctx, `
		SELECT task_id FROM agent_tasks WHERE status = 'pending' ORDER BY created_at LIMIT 1`).Scan(&taskID)
	if err == sql.ErrNoRows {
		return AgentTaskRow{}, false, nil
	}
	if err != nil {
		return AgentTaskRow{}, false, fmt.Errorf("catalog: finding pending agent task: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status = 'claimed', claimed_by = ?, claimed_at = ?, attempt = attempt + 1, updated_at = ?
		WHERE task_id = ? AND status = 'pending'`,
		workerID, now, now, taskID)
	if err != nil {
		return AgentTaskRow{}, false, fmt.Errorf("catalog: claiming agent task %s: %w", taskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return AgentTaskRow{}, false, fmt.Errorf("catalog: checking claim result for %s: %w", taskID, err)
	}
	if n == 0 {
		return AgentTaskRow{}, false, nil // lost the race to another claimer; caller just tries again next tick
	}

	task, err := s.GetAgentTask(ctx, taskID)
	return task, err == nil, err
}

// SaveAgentTaskResult records a claimed task's outcome.
func (s *SQLite) SaveAgentTaskResult(ctx context.Context, taskID, status string, resultJSON []byte, errMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status = ?, result_json = ?, error = ?, updated_at = ? WHERE task_id = ?`,
		status, nullIfEmpty(string(resultJSON)), nullIfEmpty(errMsg), now, taskID)
	if err != nil {
		return fmt.Errorf("catalog: saving agent task result: %w", err)
	}
	return mustAffectOne(res, "agent task", taskID)
}

// ReapStaleAgentTaskClaims resets any task still "claimed" past
// leaseTimeout back to "pending" — crash recovery for a worker that died
// mid-task, the same job a Temporal Activity's own timeout+retry gives
// for free.
func (s *SQLite) ReapStaleAgentTaskClaims(ctx context.Context, leaseTimeout time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-leaseTimeout).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_tasks SET status = 'pending', claimed_by = '', claimed_at = NULL
		WHERE status = 'claimed' AND claimed_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("catalog: reaping stale agent task claims: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func mustAffectOne(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("catalog: checking update result for %s %s: %w", kind, id, err)
	}
	if n == 0 {
		return fmt.Errorf("catalog: %s %s not found", kind, id)
	}
	return nil
}

func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
