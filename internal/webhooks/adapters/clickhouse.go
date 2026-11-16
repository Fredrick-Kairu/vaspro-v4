package adapters

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // registers the "clickhouse" database/sql driver
	"github.com/google/uuid"
)

// EventRow is one bus event as the events table keeps it (data model
// §8; migrations/clickhouse).
type EventRow struct {
	ID         uuid.UUID
	Type       string
	Version    uint16
	TenantID   uuid.UUID // uuid.Nil for events with no client
	Source     string    // the publishing service
	OccurredAt time.Time
	Data       string // the event's data as JSON
}

// DailyActivity is one client's wallet activity on one day, in one
// currency (task 5.6's first report).
type DailyActivity struct {
	Day                 time.Time // a date, midnight UTC
	Currency            string
	CreditedMinor       int64 // wallet.credited: top-ups and refunds
	Topups              int64 // wallet.credited of type topup
	TokenPurchasesMinor int64 // tokens.purchased: what the tokens cost
	UsageMinor          int64 // usage.posted: the tokens' value used
	UsageEvents         int64 // usage.posted: messages and other billed events
}

// TopupsByRail is one day's top-ups across clients on one rail.
type TopupsByRail struct {
	Day         time.Time
	Rail        string // "unattributed" for a top-up that names none (a match out of suspense)
	Currency    string
	Topups      int64
	AmountMinor int64
}

// EventsPerType is one day's count of one event type, every client and
// none.
type EventsPerType struct {
	Day    time.Time
	Type   string
	Events int64
}

// ClickHouse is webhooks' ClickHouse: the events table and the reports
// read from it. Reports read with FINAL, so an event inserted twice
// (the sink acknowledges only after an insert lands, so a retry can
// repeat one) is counted once even before ClickHouse merges the copies.
type ClickHouse struct {
	db *sql.DB
}

// OpenClickHouse connects to dsn (clickhouse://user:pass@host:9000/db).
func OpenClickHouse(dsn string) (*ClickHouse, error) {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("adapters: opening clickhouse: %w", err)
	}
	return &ClickHouse{db: db}, nil
}

// Ping checks the connection.
func (c *ClickHouse) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

// Close closes the connection.
func (c *ClickHouse) Close() error { return c.db.Close() }

// InsertEvents writes rows in one batch insert.
func (c *ClickHouse) InsertEvents(ctx context.Context, rows []EventRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("adapters: starting the events batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO events (id, type, version, tenant_id, source, occurred_at, data)`)
	if err != nil {
		return fmt.Errorf("adapters: preparing the events batch: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.ID, r.Type, r.Version, r.TenantID, r.Source, r.OccurredAt.UTC(), r.Data); err != nil {
			return fmt.Errorf("adapters: adding an event to the batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("adapters: inserting the events batch: %w", err)
	}
	return nil
}

const dateLayout = "2006-01-02"

// DailyActivity reports tenant's wallet activity per day in zone (an
// IANA name, e.g. Africa/Nairobi) and currency, from through to
// (dates, inclusive), oldest first. Days with no activity are absent.
func (c *ClickHouse) DailyActivity(ctx context.Context, tenant uuid.UUID, from, to time.Time, zone string) ([]DailyActivity, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT toDate(occurred_at, ?) AS day,
		       JSONExtractString(data, 'currency') AS currency,
		       toInt64(sumIf(JSONExtractInt(data, 'amount_minor'), type = 'wallet.credited')) AS credited_minor,
		       toInt64(countIf(type = 'wallet.credited' AND JSONExtractString(data, 'transaction_type') = 'topup')) AS topups,
		       toInt64(sumIf(JSONExtractInt(data, 'cost_minor'), type = 'tokens.purchased')) AS token_purchases_minor,
		       toInt64(sumIf(JSONExtractInt(data, 'value_minor'), type = 'usage.posted')) AS usage_minor,
		       toInt64(sumIf(JSONExtractInt(data, 'events'), type = 'usage.posted')) AS usage_events
		FROM events FINAL
		WHERE tenant_id = ?
		  AND type IN ('wallet.credited', 'tokens.purchased', 'usage.posted')
		  AND toDate(occurred_at, ?) BETWEEN toDate(?) AND toDate(?)
		GROUP BY day, currency
		ORDER BY day, currency`,
		zone, tenant, zone, from.Format(dateLayout), to.Format(dateLayout))
	if err != nil {
		return nil, fmt.Errorf("adapters: daily activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DailyActivity
	for rows.Next() {
		var a DailyActivity
		if err := rows.Scan(&a.Day, &a.Currency, &a.CreditedMinor, &a.Topups, &a.TokenPurchasesMinor, &a.UsageMinor, &a.UsageEvents); err != nil {
			return nil, fmt.Errorf("adapters: reading daily activity: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TopupsByRail reports top-ups (wallet.credited of type topup) across
// clients per day in zone, rail and currency, from through to.
func (c *ClickHouse) TopupsByRail(ctx context.Context, from, to time.Time, zone string) ([]TopupsByRail, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT toDate(occurred_at, ?) AS day,
		       if(JSONExtractString(data, 'rail') = '', 'unattributed', JSONExtractString(data, 'rail')) AS rail,
		       JSONExtractString(data, 'currency') AS currency,
		       toInt64(count()) AS topups,
		       toInt64(sum(JSONExtractInt(data, 'amount_minor'))) AS amount_minor
		FROM events FINAL
		WHERE type = 'wallet.credited' AND JSONExtractString(data, 'transaction_type') = 'topup'
		  AND toDate(occurred_at, ?) BETWEEN toDate(?) AND toDate(?)
		GROUP BY day, rail, currency
		ORDER BY day, rail, currency`,
		zone, zone, from.Format(dateLayout), to.Format(dateLayout))
	if err != nil {
		return nil, fmt.Errorf("adapters: top-ups by rail: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TopupsByRail
	for rows.Next() {
		var r TopupsByRail
		if err := rows.Scan(&r.Day, &r.Rail, &r.Currency, &r.Topups, &r.AmountMinor); err != nil {
			return nil, fmt.Errorf("adapters: reading top-ups by rail: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EventsPerType counts every event per UTC day and type, from through
// to.
func (c *ClickHouse) EventsPerType(ctx context.Context, from, to time.Time) ([]EventsPerType, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT toDate(occurred_at, 'UTC') AS day, type, toInt64(count()) AS events
		FROM events FINAL
		WHERE toDate(occurred_at, 'UTC') BETWEEN toDate(?) AND toDate(?)
		GROUP BY day, type
		ORDER BY day, type`,
		from.Format(dateLayout), to.Format(dateLayout))
	if err != nil {
		return nil, fmt.Errorf("adapters: events per type: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []EventsPerType
	for rows.Next() {
		var e EventsPerType
		if err := rows.Scan(&e.Day, &e.Type, &e.Events); err != nil {
			return nil, fmt.Errorf("adapters: reading events per type: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
