-- One row per dispatched search task -- the unit cmd/collector -worker
-- claims and runs. Stands in for a Temporal Activity's retry/lease
-- semantics: claimed_by/claimed_at is a plain lease, reset to 'pending'
-- by a stale-claim sweep if a worker dies mid-task (see DESIGN.md
-- "Agent loop").
CREATE TABLE agent_tasks (
	task_id      TEXT PRIMARY KEY,
	request_id   TEXT NOT NULL REFERENCES agent_requests(request_id),
	round        INTEGER NOT NULL,
	params_json  TEXT NOT NULL,
	status       TEXT NOT NULL DEFAULT 'pending', -- pending | claimed | done | failed
	claimed_by   TEXT NOT NULL DEFAULT '',
	claimed_at   TEXT,
	attempt      INTEGER NOT NULL DEFAULT 0,
	result_json  TEXT,
	error        TEXT,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
