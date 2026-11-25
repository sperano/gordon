-- Shadow mode is gone: Gordon is the only dispatcher and executes every
-- family, so no Task is kept for comparison with a legacy one. A worker
-- of the previous release still running during the rollout only fails
-- its shadow comparison pass and its retention of this table (each logged
-- and retried, neither fatal) until it is replaced.
DROP TABLE IF EXISTS shadow_tasks;
