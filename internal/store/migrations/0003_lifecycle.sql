-- Gordon 06: the Vikunja lifecycle. An action may wait for another of its
-- decision (a claim for its run's submission, a chain's move for the
-- subtask's assignment); runs are found by their Vikunja task (a drag to
-- the backlog stops the task's runs); the worker queues synthetic
-- deliveries (requeues and reconciliation), which only it can mark.

ALTER TABLE actions ADD COLUMN after_key text NOT NULL DEFAULT '';
CREATE INDEX actions_waiting ON actions (inbox_id, after_key) WHERE after_key <> '' AND state = 'pending';
CREATE INDEX runs_task ON runs ((identity->>'task_id'));
