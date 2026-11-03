-- +goose Up

-- Task 2.8: the same file is stored once per account (ROADMAP Phase 2:
-- "duplicate (same hash reused, not stored twice)"). app.UploadDocument
-- returns the existing document for a repeated hash; this index makes
-- two concurrent uploads of one file unable to both land. Per account,
-- never across accounts, so one account's files are never visible to
-- another. An addition to data model §4; the table has no rows yet.
CREATE UNIQUE INDEX documents_tenant_sha256 ON accounts.documents (tenant_id, sha256);

-- +goose Down
DROP INDEX accounts.documents_tenant_sha256;
