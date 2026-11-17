package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrValueInvalid wraps every refused field or attribute value.
var ErrValueInvalid = errors.New("value not valid")

// Attribute types (attribute_definitions.type).
const (
	TypeText    = "text"
	TypeNumber  = "number"
	TypeDate    = "date"
	TypeBoolean = "boolean"
)

// MaxAttributes is how many attributes a client may define.
const MaxAttributes = 50

var (
	attributeKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	decimal      = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
)

// ValidAttributeKey: lower case, a letter first, then letters, digits
// and underscores, at most 40 (data model §10). Keys go into SQL/JSON
// paths, so nothing else is allowed.
func ValidAttributeKey(key string) bool { return attributeKey.MatchString(key) }

// ValidAttributeType reports whether typ is an attribute type.
func ValidAttributeType(typ string) bool {
	switch typ {
	case TypeText, TypeNumber, TypeDate, TypeBoolean:
		return true
	}
	return false
}

// AttributeValue checks raw against an attribute's type and returns the
// JSON stored in contacts.attributes: a string for text and dates, a
// JSON number kept as its decimal text (never a float) for numbers, and
// true or false. A blank value is nil: no value.
func AttributeValue(typ, raw string) (json.RawMessage, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	switch typ {
	case TypeText:
		if utf8.RuneCountInString(s) > 255 || hasControl(s) {
			return nil, fmt.Errorf("%w: text is at most 255 characters", ErrValueInvalid)
		}
		return jsonString(s), nil
	case TypeNumber:
		if len(s) > 30 || !decimal.MatchString(s) {
			return nil, fmt.Errorf("%w: not a number such as 42 or -3.50", ErrValueInvalid)
		}
		return json.RawMessage(s), nil
	case TypeDate:
		d, err := time.Parse(time.DateOnly, s)
		if err != nil || d.Format(time.DateOnly) != s {
			return nil, fmt.Errorf("%w: not a date as YYYY-MM-DD", ErrValueInvalid)
		}
		return jsonString(s), nil
	case TypeBoolean:
		switch strings.ToLower(s) {
		case "true", "yes":
			return json.RawMessage("true"), nil
		case "false", "no":
			return json.RawMessage("false"), nil
		}
		return nil, fmt.Errorf("%w: not true, false, yes or no", ErrValueInvalid)
	}
	return nil, fmt.Errorf("%w: unknown attribute type %q", ErrValueInvalid, typ)
}

func jsonString(s string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return json.RawMessage(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// ContactFields are a contact's own fields; empty means none.
type ContactFields struct {
	FirstName, LastName, Email string
}

// NormalizeContactFields trims the fields and checks them: names at
// most 100 characters, an email a bare address of at most 254.
func NormalizeContactFields(f ContactFields) (ContactFields, error) {
	f = ContactFields{FirstName: strings.TrimSpace(f.FirstName), LastName: strings.TrimSpace(f.LastName), Email: strings.TrimSpace(f.Email)}
	for _, name := range []string{f.FirstName, f.LastName} {
		if utf8.RuneCountInString(name) > 100 || hasControl(name) {
			return ContactFields{}, fmt.Errorf("%w: a name is at most 100 characters", ErrValueInvalid)
		}
	}
	if f.Email != "" {
		addr, err := mail.ParseAddress(f.Email)
		if err != nil || addr.Name != "" || addr.Address != f.Email || len(f.Email) > 254 {
			return ContactFields{}, fmt.Errorf("%w: not an email address", ErrValueInvalid)
		}
	}
	return f, nil
}
