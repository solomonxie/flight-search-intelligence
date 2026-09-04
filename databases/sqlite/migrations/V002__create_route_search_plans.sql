-- One row per routesearch request -- the audit trail from DESIGN.md
-- "Audit trail" (candidates considered, kept, or pruned, and why).
CREATE TABLE route_search_plans (
	id          TEXT PRIMARY KEY,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	status      TEXT NOT NULL,
	plan_json   TEXT NOT NULL
);
