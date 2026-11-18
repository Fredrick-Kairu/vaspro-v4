package domain

import (
	"errors"
	"strings"
)

var (
	// ErrNoPhoneColumn: no column holds the number, so nothing can be
	// imported (VAL-4022).
	ErrNoPhoneColumn = errors.New("no column holds the phone number: name one phone, msisdn, mobile, number or phone_number")
	// ErrDuplicateColumn: two columns map to one field.
	ErrDuplicateColumn = errors.New("two columns are for the same field")
)

// AttributeDef is a client's attribute (attribute_definitions).
type AttributeDef struct {
	Key, Label, Type string
}

// HeaderMap says which column holds what; -1 is no column.
type HeaderMap struct {
	Phone, FirstName, LastName, Email int
	Attributes                        map[int]AttributeDef // by column
}

var fieldNames = map[string]string{
	"phone": "phone", "msisdn": "phone", "mobile": "phone", "number": "phone",
	"phone number": "phone", "mobile number": "phone",
	"first name": "first_name", "firstname": "first_name", "name": "first_name",
	"last name": "last_name", "lastname": "last_name", "surname": "last_name",
	"email": "email", "e mail": "email", "email address": "email",
}

// headerKey makes a header comparable: no byte-order mark, lower case,
// and spaces, underscores and hyphens as single spaces.
func headerKey(h string) string {
	h = strings.TrimPrefix(h, "\ufeff")
	h = strings.Map(func(c rune) rune {
		if c == '_' || c == '-' {
			return ' '
		}
		return c
	}, strings.ToLower(h))
	return strings.Join(strings.Fields(h), " ")
}

// MatchHeaders maps an import's header row to fields (data model §10):
// the number under any of its usual names, the contact fields, then an
// attribute by key or label. It returns the columns it couldn't place,
// which an import reports and ignores.
func MatchHeaders(headers []string, defs []AttributeDef) (HeaderMap, []int, error) {
	m := HeaderMap{Phone: -1, FirstName: -1, LastName: -1, Email: -1, Attributes: map[int]AttributeDef{}}
	attrs := map[string]AttributeDef{}
	for _, d := range defs {
		attrs[headerKey(d.Key)] = d
		attrs[headerKey(d.Label)] = d
	}
	seen := map[string]bool{}
	var unknown []int
	for i, h := range headers {
		k := headerKey(h)
		field, isField := fieldNames[k]
		def, isAttr := attrs[k]
		switch {
		case isField:
		case isAttr:
			field = "attributes." + def.Key
		default:
			unknown = append(unknown, i)
			continue
		}
		if seen[field] {
			return HeaderMap{}, nil, ErrDuplicateColumn
		}
		seen[field] = true
		switch field {
		case "phone":
			m.Phone = i
		case "first_name":
			m.FirstName = i
		case "last_name":
			m.LastName = i
		case "email":
			m.Email = i
		default:
			m.Attributes[i] = def
		}
	}
	if m.Phone < 0 {
		return HeaderMap{}, nil, ErrNoPhoneColumn
	}
	return m, unknown, nil
}
