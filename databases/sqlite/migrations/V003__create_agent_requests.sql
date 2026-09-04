-- One row per in-flight/finalized travel request -- durable state for
-- the agent loop (DESIGN.md "Agent loop"). No workflow engine backs this:
-- the row itself IS the checkpoint, advanced by internal/agents.Advance-
-- Request on each poll tick (cmd/email-intake -worker) -- a crash between
-- ticks loses nothing, since nothing lives only in process memory.
CREATE TABLE agent_requests (
	request_id      TEXT PRIMARY KEY,
	spec_json       TEXT NOT NULL,
	rounds_json     TEXT NOT NULL,
	status          TEXT NOT NULL, -- awaiting_decision | dispatched | deferred | finalized
	deferred_until  TEXT,
	email_body      TEXT,
	finalized_by    TEXT,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
