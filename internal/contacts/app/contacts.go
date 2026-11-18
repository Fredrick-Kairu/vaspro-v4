package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/store"
)

// Contact is a contact as the API shows it.
type Contact struct {
	ID                         uuid.UUID
	Phone                      string
	FirstName, LastName, Email string
	Attributes                 map[string]json.RawMessage
	Source                     string
	CreatedAt, UpdatedAt       time.Time
}

// ContactInput adds a contact: the number in any of the country's
// formats, its fields, and attribute values as text.
type ContactInput struct {
	Phone      string
	Fields     domain.ContactFields
	Attributes map[string]string
}

// ContactPatch changes a contact: a nil field is left alone, an empty
// one cleared; an attribute set to nil is removed.
type ContactPatch struct {
	FirstName, LastName, Email *string
	Attributes                 map[string]*string
}

func toContact(c store.ContactsContact) Contact {
	attrs := map[string]json.RawMessage{}
	_ = json.Unmarshal(c.Attributes, &attrs) // the column is a JSON object (CHECK)
	return Contact{
		ID: c.ID, Phone: c.PhoneE164, FirstName: c.FirstName.String, LastName: c.LastName.String,
		Email: c.Email.String, Attributes: attrs, Source: c.Source, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// attributeValues checks values against the account's definitions and
// returns them as a JSON object; a blank value is left out.
func attributeValues(defs map[string]domain.AttributeDef, values map[string]string) ([]byte, error) {
	out := map[string]json.RawMessage{}
	for key, raw := range values {
		def, ok := defs[key]
		if !ok {
			return nil, fmt.Errorf("%w: no attribute %q in this account", ErrValue, key)
		}
		v, err := domain.AttributeValue(def.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrValue, key, err)
		}
		if v != nil {
			out[key] = v
		}
	}
	return json.Marshal(out)
}

func loadDefs(ctx context.Context, q *store.Queries) (map[string]domain.AttributeDef, error) {
	rows, err := q.ListAttributeDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	defs := make(map[string]domain.AttributeDef, len(rows))
	for _, r := range rows {
		defs[r.Key] = domain.AttributeDef{Key: r.Key, Label: r.Label, Type: r.Type}
	}
	return defs, nil
}

// CreateContact adds a contact, or merges into the one with its number
// (data model §10); created says which.
func (s *Service) CreateContact(ctx context.Context, user, tenant uuid.UUID, in ContactInput) (Contact, bool, error) {
	country, err := s.requireTenant(ctx, user, tenant, PermManage)
	if err != nil {
		return Contact{}, false, err
	}
	rules, err := s.phoneRules(country)
	if err != nil {
		return Contact{}, false, err
	}
	phone, err := domain.NormalizePhone(rules, in.Phone)
	if err != nil {
		return Contact{}, false, fmt.Errorf("%w: %v", ErrPhone, err)
	}
	fields, err := domain.NormalizeContactFields(in.Fields)
	if err != nil {
		return Contact{}, false, fmt.Errorf("%w: %v", ErrValue, err)
	}
	id, err := ids.New()
	if err != nil {
		return Contact{}, false, err
	}
	var out store.ContactsContact
	created := false
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		defs, err := loadDefs(ctx, q)
		if err != nil {
			return err
		}
		attrs, err := attributeValues(defs, in.Attributes)
		if err != nil {
			return err
		}
		out, err = q.InsertContactIfNew(ctx, store.InsertContactIfNewParams{
			ID: id, TenantID: tenant, PhoneE164: phone, FirstName: text(fields.FirstName), LastName: text(fields.LastName),
			Email: text(fields.Email), Attributes: attrs, Source: "api", CreatedBy: pgtype.UUID{Bytes: user, Valid: true},
		})
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = q.MergeContact(ctx, store.MergeContactParams{
			FirstName: text(fields.FirstName), LastName: text(fields.LastName), Email: text(fields.Email),
			Attributes: attrs, At: s.clk.Now(), PhoneE164: phone,
		})
		return err
	})
	if err != nil {
		return Contact{}, false, err
	}
	return toContact(out), created, nil
}

// GetContact reads one contact.
func (s *Service) GetContact(ctx context.Context, user, tenant, id uuid.UUID) (Contact, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return Contact{}, err
	}
	var c store.ContactsContact
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = store.New(tx).GetContact(ctx, id)
		return notFound(err, "contact")
	})
	return toContact(c), err
}

// ListContacts pages the account's contacts, or one group's, newest first.
func (s *Service) ListContacts(ctx context.Context, user, tenant uuid.UUID, group *uuid.UUID, page Page) ([]Contact, string, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
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
	params := store.ListContactsParams{PageSize: size + 1}
	if group != nil {
		params.GroupID = pgtype.UUID{Bytes: *group, Valid: true}
	}
	if after != nil {
		params.AfterCreated = pgtype.Timestamptz{Time: *after, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: afterID, Valid: true}
	}
	var rows []store.ContactsContact
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if group != nil {
			if _, err := q.GetGroup(ctx, *group); err != nil {
				return notFound(err, "group")
			}
		}
		var err error
		rows, err = q.ListContacts(ctx, params)
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

// UpdateContact changes a contact's fields and attributes.
func (s *Service) UpdateContact(ctx context.Context, user, tenant, id uuid.UUID, p ContactPatch) (Contact, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return Contact{}, err
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	fields, err := domain.NormalizeContactFields(domain.ContactFields{FirstName: deref(p.FirstName), LastName: deref(p.LastName), Email: deref(p.Email)})
	if err != nil {
		return Contact{}, fmt.Errorf("%w: %v", ErrValue, err)
	}
	set := map[string]string{}
	remove := []string{}
	for k, v := range p.Attributes {
		if v == nil {
			remove = append(remove, k)
			continue
		}
		set[k] = *v
	}
	var c store.ContactsContact
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		defs, err := loadDefs(ctx, q)
		if err != nil {
			return err
		}
		attrs, err := attributeValues(defs, set)
		if err != nil {
			return err
		}
		c, err = q.UpdateContact(ctx, store.UpdateContactParams{
			SetFirstName: p.FirstName != nil, FirstName: text(fields.FirstName),
			SetLastName: p.LastName != nil, LastName: text(fields.LastName),
			SetEmail: p.Email != nil, Email: text(fields.Email),
			SetAttributes: attrs, RemoveKeys: remove, At: s.clk.Now(), ID: id,
		})
		return notFound(err, "contact")
	})
	return toContact(c), err
}

// DeleteContact deletes a contact, its attributes and its memberships;
// its number's consent and suppressions stay (data model §10).
func (s *Service) DeleteContact(ctx context.Context, user, tenant, id uuid.UUID) error {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		n, err := store.New(tx).DeleteContact(ctx, id)
		if err == nil && n == 0 {
			return fmt.Errorf("%w: contact", ErrNotFound)
		}
		return err
	})
}

// AttributeDef is a contact attribute as the API shows it.
type AttributeDef struct {
	domain.AttributeDef
	CreatedAt time.Time
}

// CreateAttribute defines a contact attribute: at most 50 per account.
func (s *Service) CreateAttribute(ctx context.Context, user, tenant uuid.UUID, def domain.AttributeDef) (AttributeDef, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return AttributeDef{}, err
	}
	if !domain.ValidAttributeKey(def.Key) || !domain.ValidAttributeType(def.Type) || def.Label == "" || len([]rune(def.Label)) > 100 {
		return AttributeDef{}, fmt.Errorf("%w: a key of lower-case letters, digits and underscores, a label and a type", ErrValidation)
	}
	var out store.ContactsAttributeDefinition
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.CountAttributeDefinitions(ctx)
		if err != nil {
			return err
		}
		if n >= domain.MaxAttributes {
			return fmt.Errorf("%w: at most %d attributes per account", ErrValidation, domain.MaxAttributes)
		}
		out, err = q.CreateAttributeDefinition(ctx, store.CreateAttributeDefinitionParams{TenantID: tenant, Key: def.Key, Label: def.Label, Type: def.Type})
		if isUnique(err) {
			return fmt.Errorf("%w: attribute %q exists", ErrValidation, def.Key)
		}
		return err
	})
	return AttributeDef{AttributeDef: domain.AttributeDef{Key: out.Key, Label: out.Label, Type: out.Type}, CreatedAt: out.CreatedAt}, err
}

// ListAttributes lists the account's attributes, by key.
func (s *Service) ListAttributes(ctx context.Context, user, tenant uuid.UUID) ([]AttributeDef, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return nil, err
	}
	var rows []store.ContactsAttributeDefinition
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListAttributeDefinitions(ctx)
		return err
	})
	out := make([]AttributeDef, len(rows))
	for i, r := range rows {
		out[i] = AttributeDef{AttributeDef: domain.AttributeDef{Key: r.Key, Label: r.Label, Type: r.Type}, CreatedAt: r.CreatedAt}
	}
	return out, err
}

// DeleteAttribute deletes an attribute and its values on every contact.
func (s *Service) DeleteAttribute(ctx context.Context, user, tenant uuid.UUID, key string) error {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.DeleteAttributeDefinition(ctx, key)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: attribute", ErrNotFound)
		}
		_, err = q.RemoveAttributeFromContacts(ctx, store.RemoveAttributeFromContactsParams{Key: key, At: s.clk.Now()})
		return err
	})
}

// Group is a group as the API shows it.
type Group struct {
	ID                   uuid.UUID
	Name, Description    string
	Members              int64
	CreatedAt, UpdatedAt time.Time
}

// CreateGroup creates a group; names are unique ignoring case.
func (s *Service) CreateGroup(ctx context.Context, user, tenant uuid.UUID, name, description string) (Group, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return Group{}, err
	}
	if name == "" || len([]rune(name)) > 100 || len([]rune(description)) > 500 {
		return Group{}, fmt.Errorf("%w: a name of 1 to 100 characters", ErrValidation)
	}
	id, err := ids.New()
	if err != nil {
		return Group{}, err
	}
	var g store.ContactsGroup
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = store.New(tx).CreateGroup(ctx, store.CreateGroupParams{
			ID: id, TenantID: tenant, Name: name, Description: text(description), CreatedBy: pgtype.UUID{Bytes: user, Valid: true},
		})
		if isUnique(err) {
			return fmt.Errorf("%w: a group named %q exists", ErrValidation, name)
		}
		return err
	})
	return Group{ID: g.ID, Name: g.Name, Description: g.Description.String, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}, err
}

// ListGroups lists the account's groups by name, with their sizes.
func (s *Service) ListGroups(ctx context.Context, user, tenant uuid.UUID) ([]Group, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return nil, err
	}
	var rows []store.ListGroupsRow
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListGroups(ctx)
		return err
	})
	out := make([]Group, len(rows))
	for i, g := range rows {
		out[i] = Group{ID: g.ID, Name: g.Name, Description: g.Description.String, Members: g.Members, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
	}
	return out, err
}

// GetGroup reads one group with its size.
func (s *Service) GetGroup(ctx context.Context, user, tenant, id uuid.UUID) (Group, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return Group{}, err
	}
	var g store.GetGroupWithCountRow
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = store.New(tx).GetGroupWithCount(ctx, id)
		return notFound(err, "group")
	})
	return Group{ID: g.ID, Name: g.Name, Description: g.Description.String, Members: g.Members, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}, err
}

// UpdateGroup renames or describes a group; nil leaves a field alone,
// an empty description clears it.
func (s *Service) UpdateGroup(ctx context.Context, user, tenant, id uuid.UUID, name, description *string) (Group, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return Group{}, err
	}
	if name != nil && (*name == "" || len([]rune(*name)) > 100) || description != nil && len([]rune(*description)) > 500 {
		return Group{}, fmt.Errorf("%w: a name of 1 to 100 characters", ErrValidation)
	}
	params := store.UpdateGroupParams{ID: id, At: s.clk.Now(), SetDescription: description != nil}
	if name != nil {
		params.Name = pgtype.Text{String: *name, Valid: true}
	}
	if description != nil {
		params.Description = text(*description)
	}
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).UpdateGroup(ctx, params)
		if isUnique(err) {
			return fmt.Errorf("%w: a group with that name exists", ErrValidation)
		}
		return notFound(err, "group")
	})
	if err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, user, tenant, id)
}

// DeleteGroup deletes a group; its contacts stay.
func (s *Service) DeleteGroup(ctx context.Context, user, tenant, id uuid.UUID) error {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		n, err := store.New(tx).DeleteGroup(ctx, id)
		if err == nil && n == 0 {
			return fmt.Errorf("%w: group", ErrNotFound)
		}
		return err
	})
}

// AddGroupMembers adds up to 1,000 contacts to a group; any unknown
// contact adds nothing.
func (s *Service) AddGroupMembers(ctx context.Context, user, tenant, group uuid.UUID, contacts []uuid.UUID) (int64, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return 0, err
	}
	if len(contacts) == 0 || len(contacts) > 1000 {
		return 0, fmt.Errorf("%w: 1 to 1,000 contacts", ErrValidation)
	}
	unique := map[uuid.UUID]bool{}
	for _, c := range contacts {
		unique[c] = true
	}
	var added int64
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetGroup(ctx, group); err != nil {
			return notFound(err, "group")
		}
		n, err := q.CountContactsByIDs(ctx, contacts)
		if err != nil {
			return err
		}
		if n != int64(len(unique)) {
			return fmt.Errorf("%w: contact", ErrNotFound)
		}
		added, err = q.AddGroupMembers(ctx, store.AddGroupMembersParams{TenantID: tenant, GroupID: group, ContactIds: contacts})
		return err
	})
	return added, err
}

// RemoveGroupMember takes a contact out of a group.
func (s *Service) RemoveGroupMember(ctx context.Context, user, tenant, group, contact uuid.UUID) error {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetGroup(ctx, group); err != nil {
			return notFound(err, "group")
		}
		_, err := q.RemoveGroupMember(ctx, store.RemoveGroupMemberParams{GroupID: group, ContactID: contact})
		return err
	})
}

// notFound turns pgx.ErrNoRows into ErrNotFound.
func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return err
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
