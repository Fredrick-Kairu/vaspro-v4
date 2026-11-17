package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/store"
)

// ErrFilter: a segment's filter isn't valid, or names an attribute or
// group the account doesn't have, or no longer has (VAL-4025).
var ErrFilter = errors.New("app: segment filter not valid")

// Segment is a saved filter as the API shows it.
type Segment struct {
	ID                   uuid.UUID
	Name                 string
	Filter               json.RawMessage
	CreatedAt, UpdatedAt time.Time
}

func toSegment(s store.ContactsSegment) Segment {
	return Segment{ID: s.ID, Name: s.Name, Filter: s.Filter, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

func validSegmentName(name string) bool {
	n := len([]rune(name))
	return n >= 1 && n <= 100
}

// compileFilter checks filter against the account's attributes and
// groups, in the caller's transaction, and compiles it.
func compileFilter(ctx context.Context, q *store.Queries, filter []byte) (domain.CompiledSegment, error) {
	defs, err := loadDefs(ctx, q)
	if err != nil {
		return domain.CompiledSegment{}, err
	}
	compiled, err := domain.CompileSegment(filter, defs)
	if err != nil {
		return domain.CompiledSegment{}, fmt.Errorf("%w: %s", ErrFilter, strings.TrimPrefix(err.Error(), domain.ErrFilterInvalid.Error()+": "))
	}
	if len(compiled.Groups) > 0 {
		groups := make([]uuid.UUID, len(compiled.Groups))
		for i, g := range compiled.Groups {
			groups[i] = uuid.MustParse(g) // the domain gave canonical IDs
		}
		n, err := q.CountGroupsByIDs(ctx, groups)
		if err != nil {
			return domain.CompiledSegment{}, err
		}
		if n != int64(len(groups)) {
			return domain.CompiledSegment{}, fmt.Errorf("%w: a group it names isn't in this account", ErrFilter)
		}
	}
	return compiled, nil
}

// compactFilter stores the client's filter without its spacing.
func compactFilter(filter []byte) ([]byte, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, filter); err != nil {
		return nil, fmt.Errorf("%w: not JSON", ErrFilter)
	}
	return b.Bytes(), nil
}

// CreateSegment saves a filter after checking it (data model §10);
// names are unique ignoring case.
func (s *Service) CreateSegment(ctx context.Context, user, tenant uuid.UUID, name string, filter json.RawMessage) (Segment, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return Segment{}, err
	}
	if !validSegmentName(name) {
		return Segment{}, fmt.Errorf("%w: a name of 1 to 100 characters", ErrValidation)
	}
	stored, err := compactFilter(filter)
	if err != nil {
		return Segment{}, err
	}
	id, err := ids.New()
	if err != nil {
		return Segment{}, err
	}
	var out store.ContactsSegment
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := compileFilter(ctx, q, stored); err != nil {
			return err
		}
		out, err = q.CreateSegment(ctx, store.CreateSegmentParams{
			ID: id, TenantID: tenant, Name: name, Filter: stored, CreatedBy: pgtype.UUID{Bytes: user, Valid: true},
		})
		if isUnique(err) {
			return fmt.Errorf("%w: a segment named %q exists", ErrValidation, name)
		}
		return err
	})
	return toSegment(out), err
}

// GetSegment reads one segment.
func (s *Service) GetSegment(ctx context.Context, user, tenant, id uuid.UUID) (Segment, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return Segment{}, err
	}
	var out store.ContactsSegment
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = store.New(tx).GetSegment(ctx, id)
		return notFound(err, "segment")
	})
	return toSegment(out), err
}

// ListSegments lists the account's segments by name.
func (s *Service) ListSegments(ctx context.Context, user, tenant uuid.UUID) ([]Segment, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return nil, err
	}
	var rows []store.ContactsSegment
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListSegments(ctx)
		return err
	})
	out := make([]Segment, len(rows))
	for i, r := range rows {
		out[i] = toSegment(r)
	}
	return out, err
}

// UpdateSegment renames a segment or replaces its filter, checked as
// on creation; nil leaves either alone.
func (s *Service) UpdateSegment(ctx context.Context, user, tenant, id uuid.UUID, name *string, filter json.RawMessage) (Segment, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return Segment{}, err
	}
	params := store.UpdateSegmentParams{ID: id, At: s.clk.Now()}
	if name != nil {
		if !validSegmentName(*name) {
			return Segment{}, fmt.Errorf("%w: a name of 1 to 100 characters", ErrValidation)
		}
		params.Name = pgtype.Text{String: *name, Valid: true}
	}
	if filter != nil {
		stored, err := compactFilter(filter)
		if err != nil {
			return Segment{}, err
		}
		params.Filter = stored
	}
	var out store.ContactsSegment
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetSegment(ctx, id); err != nil {
			return notFound(err, "segment")
		}
		if params.Filter != nil {
			if _, err := compileFilter(ctx, q, params.Filter); err != nil {
				return err
			}
		}
		var err error
		out, err = q.UpdateSegment(ctx, params)
		if isUnique(err) {
			return fmt.Errorf("%w: a segment with that name exists", ErrValidation)
		}
		return notFound(err, "segment")
	})
	return toSegment(out), err
}

// DeleteSegment deletes a segment; its contacts are untouched.
func (s *Service) DeleteSegment(ctx context.Context, user, tenant, id uuid.UUID) error {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		n, err := store.New(tx).DeleteSegment(ctx, id)
		if err == nil && n == 0 {
			return fmt.Errorf("%w: segment", ErrNotFound)
		}
		return err
	})
}

// segmentQuery is what the fixed query needs to evaluate a segment.
type segmentQuery struct {
	compiled domain.CompiledSegment
	zone     string
}

// loadSegment reads a segment and compiles its filter against the
// account as it is now, so an attribute or group deleted since is
// refused (data model §10).
func (s *Service) loadSegment(ctx context.Context, q *store.Queries, country string, id uuid.UUID) (segmentQuery, error) {
	seg, err := q.GetSegment(ctx, id)
	if err != nil {
		return segmentQuery{}, notFound(err, "segment")
	}
	compiled, err := compileFilter(ctx, q, seg.Filter)
	if err != nil {
		return segmentQuery{}, err
	}
	zone, err := s.zone(country)
	if err != nil {
		return segmentQuery{}, err
	}
	return segmentQuery{compiled: compiled, zone: zone}, nil
}

// zone is the account's country's time zone: a contact's created_at is
// compared as a date there.
func (s *Service) zone(country string) (string, error) {
	p, ok := s.packs[country]
	if !ok {
		return "", fmt.Errorf("app: no pack for country %q", country)
	}
	return p.Timezone, nil
}

// CountSegment counts the contacts a segment matches now.
func (s *Service) CountSegment(ctx context.Context, user, tenant, id uuid.UUID) (int64, error) {
	country, err := s.requireTenant(ctx, user, tenant, PermRead)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		sq, err := s.loadSegment(ctx, q, country, id)
		if err != nil {
			return err
		}
		n, err = q.CountSegmentContacts(ctx, store.CountSegmentContactsParams{
			TenantID: tenant, Zone: sq.zone, WithGroups: sq.compiled.UsesGroups, Path: sq.compiled.Path, Vars: sq.compiled.Vars,
		})
		return err
	})
	return n, err
}

// ListSegmentContacts pages the contacts a segment matches now, newest
// first.
func (s *Service) ListSegmentContacts(ctx context.Context, user, tenant, id uuid.UUID, page Page) ([]Contact, string, error) {
	country, err := s.requireTenant(ctx, user, tenant, PermRead)
	if err != nil {
		return nil, "", err
	}
	size, err := page.size()
	if err != nil {
		return nil, "", err
	}
	after, afterID, err := parseTimeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	var rows []store.ContactsContact
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = s.segmentPage(ctx, store.New(tx), tenant, country, id, after, afterID, size+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > int(size) {
		rows = rows[:size]
		last := rows[len(rows)-1]
		next = timeCursor(last.CreatedAt, last.ID)
	}
	out := make([]Contact, len(rows))
	for i, r := range rows {
		out[i] = toContact(r)
	}
	return out, next, nil
}

// segmentPage reads up to limit of a segment's contacts after a
// position, newest first, evaluating its filter as the account is now.
func (s *Service) segmentPage(ctx context.Context, q *store.Queries, tenant uuid.UUID, country string, id uuid.UUID, after *time.Time, afterID uuid.UUID, limit int32) ([]store.ContactsContact, error) {
	sq, err := s.loadSegment(ctx, q, country, id)
	if err != nil {
		return nil, err
	}
	params := store.ListSegmentContactsParams{
		TenantID: tenant, Zone: sq.zone, WithGroups: sq.compiled.UsesGroups, Path: sq.compiled.Path, Vars: sq.compiled.Vars, PageSize: limit,
	}
	if after != nil {
		params.AfterCreated = pgtype.Timestamptz{Time: *after, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: afterID, Valid: true}
	}
	return q.ListSegmentContacts(ctx, params)
}
