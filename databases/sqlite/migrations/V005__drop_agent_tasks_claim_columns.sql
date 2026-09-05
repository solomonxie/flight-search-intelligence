-- claimed_by/claimed_at/attempt were the poll-loop version's claim/lease
-- columns (see V004) -- now dead weight: cmd/collector -worker no longer
-- polls and claims a row, it loads a task by id straight off a Kafka
-- message, and Kafka's own consumer-group commit-after-work semantics
-- (internal/kafka) already cover "don't double-process" and "redeliver if
-- a worker crashes mid-task". See DESIGN.md "Collector task dispatch".
ALTER TABLE agent_tasks DROP COLUMN claimed_by;
ALTER TABLE agent_tasks DROP COLUMN claimed_at;
ALTER TABLE agent_tasks DROP COLUMN attempt;
