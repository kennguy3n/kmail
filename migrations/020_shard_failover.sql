-- KMail — Phase 4: Stalwart shard failover ordering.
--
-- Adds `shard_failover_config` so the JMAP proxy can fail over
-- from the primary shard to a configured backup when the primary
-- returns 5xx or trips the circuit breaker.
--
-- One row per (shard, backup) pair. `priority` orders the backups
-- when multiple exist (lower = preferred). The composite primary
-- key prevents duplicate entries.

BEGIN;

CREATE TABLE shard_failover_config (
    shard_id        UUID NOT NULL REFERENCES stalwart_shards(id) ON DELETE CASCADE,
    backup_shard_id UUID NOT NULL REFERENCES stalwart_shards(id) ON DELETE CASCADE,
    priority        INT  NOT NULL DEFAULT 100,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (shard_id, backup_shard_id),
    CHECK (shard_id <> backup_shard_id)
);

CREATE INDEX shard_failover_config_priority_idx
    ON shard_failover_config (shard_id, priority);

COMMIT;
