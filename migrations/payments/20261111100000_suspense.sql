-- +goose Up

-- Task 4.5 (OQ-22): a paybill payment that names no client goes to
-- suspense in the ledger until staff match it. The posting into
-- suspense is recorded apart from ledger_transaction_id, which stays
-- the posting that credited a client's wallet: a match moves the money
-- out of suspense only once it's in, so it's never both in suspense and
-- credited from provider clearing. An addition to data model §7.
ALTER TABLE payments.collection_notices ADD COLUMN suspense_transaction_id uuid;
GRANT UPDATE (suspense_transaction_id) ON payments.collection_notices TO payments_service, payments_system;

-- +goose Down
REVOKE UPDATE (suspense_transaction_id) ON payments.collection_notices FROM payments_service, payments_system;
ALTER TABLE payments.collection_notices DROP COLUMN suspense_transaction_id;
