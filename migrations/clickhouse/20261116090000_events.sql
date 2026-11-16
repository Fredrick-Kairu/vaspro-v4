-- +goose Up

-- Task 5.6: every bus event, copied from data model §8 (OQ-23). The
-- sink inserts in batches and acknowledges only after an insert lands,
-- so an event can arrive twice; ReplacingMergeTree collapses copies that
-- share the sorting key, and the event ID is part of it. Reports read
-- with FINAL so a copy not yet merged away is never counted.
CREATE TABLE events (
  id           UUID,
  type         LowCardinality(String),
  version      UInt16,
  tenant_id    UUID,                          -- the nil UUID for events with no client
  source       LowCardinality(String),        -- the publishing service
  occurred_at  DateTime64(3, 'UTC'),
  data         String,                        -- the event's data as JSON
  ingested_at  DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant_id, type, occurred_at, id);

-- +goose Down
DROP TABLE events;
