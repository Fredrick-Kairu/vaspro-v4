package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ErrFilterInvalid wraps every refused segment filter (VAL-4025).
var ErrFilterInvalid = errors.New("segment filter not valid")

// Segment filter limits (data model §10).
const (
	MaxSegmentConditions = 20
	MaxSegmentDepth      = 2
	MaxSegmentInValues   = 100
	MaxSegmentFilterSize = 16 << 10
)

// CompiledSegment is a filter as one fixed query runs it: a SQL/JSON
// path over each contact's document (its fields, attributes and group
// IDs) and the path's variables.
type CompiledSegment struct {
	// Path holds only our own syntax, defined attribute keys and, for
	// contains, a quoted literal (like_regex takes no variable).
	Path string
	// Vars is a JSON object of every other value: $v1, $v2…
	Vars []byte
	// Groups are the group IDs the filter names, sorted; the caller
	// checks they are the account's.
	Groups []string
	// UsesGroups says the document needs the contact's group IDs.
	UsesGroups bool
	// Attributes are the attribute keys the filter names, sorted.
	Attributes []string
}

// Field types a filter compares; attributes take their definition's.
const typeGroup = "group"

var fieldTypes = map[string]string{
	"first_name": TypeText, "last_name": TypeText, "email": TypeText, "created_at": TypeDate, "group": typeGroup,
}

// opsByType are the operators each type allows (data model §10).
var opsByType = map[string]map[string]bool{
	TypeText:    set("eq", "neq", "in", "contains", "starts_with", "exists"),
	TypeNumber:  set("eq", "neq", "gt", "gte", "lt", "lte", "between", "exists"),
	TypeDate:    set("eq", "before", "after", "between", "exists"),
	TypeBoolean: set("eq", "exists"),
	typeGroup:   set("in", "not_in"),
}

var comparisons = map[string]string{
	"eq": "==", "neq": "!=", "gt": ">", "gte": ">=", "lt": "<", "lte": "<=", "before": "<", "after": ">",
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

type compiler struct {
	defs       map[string]AttributeDef
	vars       map[string]json.RawMessage
	groups     map[string]bool
	attrs      map[string]bool
	conditions int
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrFilterInvalid}, args...)...)
}

// CompileSegment checks a filter against the account's attribute
// definitions and turns it into a SQL/JSON path with its values as
// variables. The top is an "all" or "any" group; groups nest at most
// two levels, with at most 20 conditions in all.
func CompileSegment(filter []byte, defs map[string]AttributeDef) (CompiledSegment, error) {
	if len(filter) > MaxSegmentFilterSize {
		return CompiledSegment{}, invalid("a filter is at most %d bytes", MaxSegmentFilterSize)
	}
	c := &compiler{defs: defs, vars: map[string]json.RawMessage{}, groups: map[string]bool{}, attrs: map[string]bool{}}
	node, err := decodeNode(filter)
	if err != nil {
		return CompiledSegment{}, err
	}
	if _, ok := node["field"]; ok {
		return CompiledSegment{}, invalid(`the filter must be an "all" or "any" group`)
	}
	path, err := c.node(node, 1)
	if err != nil {
		return CompiledSegment{}, err
	}
	vars, err := json.Marshal(c.vars)
	if err != nil {
		return CompiledSegment{}, err
	}
	return CompiledSegment{Path: path, Vars: vars, Groups: sorted(c.groups), UsesGroups: len(c.groups) > 0, Attributes: sorted(c.attrs)}, nil
}

func sorted(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func decodeNode(raw []byte) (map[string]json.RawMessage, error) {
	var node map[string]json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil || node == nil {
		return nil, invalid("each part must be a JSON object")
	}
	return node, nil
}

func (c *compiler) node(node map[string]json.RawMessage, depth int) (string, error) {
	all, isAll := node["all"]
	anyOf, isAny := node["any"]
	switch {
	case isAll && isAny:
		return "", invalid(`a group is "all" or "any", not both`)
	case isAll || isAny:
		if len(node) != 1 {
			return "", invalid(`a group has only "all" or "any"`)
		}
		if depth > MaxSegmentDepth {
			return "", invalid("groups nest at most %d levels", MaxSegmentDepth)
		}
		items, join := all, " && "
		if isAny {
			items, join = anyOf, " || "
		}
		return c.group(items, join, depth)
	}
	return c.condition(node)
}

func (c *compiler) group(raw json.RawMessage, join string, depth int) (string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return "", invalid("a group is a list of at least one condition")
	}
	parts := make([]string, len(items))
	for i, item := range items {
		node, err := decodeNode(item)
		if err != nil {
			return "", err
		}
		if parts[i], err = c.node(node, depth+1); err != nil {
			return "", err
		}
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, join) + ")", nil
}

func (c *compiler) condition(node map[string]json.RawMessage) (string, error) {
	for k := range node {
		if k != "field" && k != "op" && k != "value" {
			return "", invalid("a condition has only field, op and value")
		}
	}
	c.conditions++
	if c.conditions > MaxSegmentConditions {
		return "", invalid("at most %d conditions", MaxSegmentConditions)
	}
	var field, op string
	if json.Unmarshal(node["field"], &field) != nil || json.Unmarshal(node["op"], &op) != nil {
		return "", invalid("a condition needs a field and an op")
	}
	path, typ, err := c.field(field)
	if err != nil {
		return "", err
	}
	if !opsByType[typ][op] {
		return "", invalid("%s can't use %q", field, op)
	}
	value, hasValue := node["value"]
	if op == "exists" {
		if hasValue {
			return "", invalid("exists takes no value")
		}
		return "exists(" + path + ")", nil
	}
	if !hasValue {
		return "", invalid("%s %s needs a value", field, op)
	}
	switch op {
	case "in", "not_in":
		v, err := c.list(field, typ, value)
		if err != nil {
			return "", err
		}
		cond := "(" + path + "[*] == " + v + "[*])"
		if typ != typeGroup {
			cond = "(" + path + " == " + v + "[*])"
		}
		if op == "not_in" {
			return "!" + cond, nil
		}
		return cond, nil
	case "between":
		lo, hi, err := c.between(field, typ, value)
		if err != nil {
			return "", err
		}
		return "(" + path + " >= " + lo + " && " + path + " <= " + hi + ")", nil
	case "contains":
		s, err := scalar(field, typ, value)
		if err != nil {
			return "", err
		}
		// like_regex takes a literal, not a variable: the value goes in
		// as a JSON-quoted string, read literally by flag "q".
		return "(" + path + " like_regex " + string(s) + ` flag "q")`, nil
	case "starts_with":
		v, err := c.variable(field, typ, value)
		if err != nil {
			return "", err
		}
		return "(" + path + " starts with " + v + ")", nil
	}
	v, err := c.variable(field, typ, value)
	if err != nil {
		return "", err
	}
	return "(" + path + " " + comparisons[op] + " " + v + ")", nil
}

// field gives a field's path and type. An attribute key is checked
// against its pattern and the account's definitions before it's
// quoted into the path.
func (c *compiler) field(field string) (string, string, error) {
	if key, ok := strings.CutPrefix(field, "attributes."); ok {
		def, defined := c.defs[key]
		if !ValidAttributeKey(key) || !defined {
			return "", "", invalid("no attribute %q in this account", key)
		}
		c.attrs[key] = true
		return `$.attributes."` + key + `"`, def.Type, nil
	}
	typ, ok := fieldTypes[field]
	if !ok {
		return "", "", invalid("no field %q", field)
	}
	if typ == typeGroup {
		return "$.groups", typ, nil
	}
	return "$." + field, typ, nil
}

func (c *compiler) variable(field, typ string, raw json.RawMessage) (string, error) {
	v, err := scalar(field, typ, raw)
	if err != nil {
		return "", err
	}
	return c.bind(v), nil
}

func (c *compiler) bind(v json.RawMessage) string {
	name := "v" + strconv.Itoa(len(c.vars)+1)
	c.vars[name] = v
	return "$" + name
}

func (c *compiler) list(field, typ string, raw json.RawMessage) (string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 || len(items) > MaxSegmentInValues {
		return "", invalid("%s takes a list of 1 to %d values", field, MaxSegmentInValues)
	}
	seen := map[string]bool{}
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var v json.RawMessage
		if typ == typeGroup {
			var s string
			if json.Unmarshal(item, &s) != nil {
				return "", invalid("a group is a group ID")
			}
			id, err := uuid.Parse(s)
			if err != nil || len(s) != 36 {
				return "", invalid("a group is a group ID")
			}
			c.groups[id.String()] = true
			v = jsonString(id.String())
		} else {
			var err error
			if v, err = scalar(field, typ, item); err != nil {
				return "", err
			}
		}
		if !seen[string(v)] {
			seen[string(v)] = true
			out = append(out, v)
		}
	}
	list, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return c.bind(list), nil
}

func (c *compiler) between(field, typ string, raw json.RawMessage) (string, string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 2 {
		return "", "", invalid("between takes two values")
	}
	lo, err := scalar(field, typ, items[0])
	if err != nil {
		return "", "", err
	}
	hi, err := scalar(field, typ, items[1])
	if err != nil {
		return "", "", err
	}
	if typ == TypeNumber {
		l, _ := new(big.Rat).SetString(string(lo))
		h, _ := new(big.Rat).SetString(string(hi))
		if l.Cmp(h) > 0 {
			return "", "", invalid("between's first value is the lower")
		}
	} else if string(lo) > string(hi) {
		return "", "", invalid("between's first value is the earlier")
	}
	return c.bind(lo), c.bind(hi), nil
}

// scalar checks one value against a type and returns it as JSON:
// text as a string, a number as its decimal text (never a float), a
// date as YYYY-MM-DD, a boolean as true or false.
func scalar(field, typ string, raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	var s string
	isString := json.Unmarshal(raw, &s) == nil
	switch typ {
	case TypeText:
		if !isString || s == "" || utf8.RuneCountInString(s) > 255 || hasControl(s) {
			return nil, invalid("%s takes text of 1 to 255 characters", field)
		}
		if field == "email" {
			s = strings.ToLower(s)
		}
		return jsonString(s), nil
	case TypeNumber:
		text := string(raw)
		if isString {
			text = s
		}
		if len(text) > 40 || !decimal.MatchString(text) {
			return nil, invalid("%s takes a number such as 42 or -3.50", field)
		}
		return json.RawMessage(text), nil
	case TypeDate:
		d, err := time.Parse(time.DateOnly, s)
		if !isString || err != nil || d.Format(time.DateOnly) != s {
			return nil, invalid("%s takes a date as YYYY-MM-DD", field)
		}
		return jsonString(s), nil
	case TypeBoolean:
		if string(raw) != "true" && string(raw) != "false" {
			return nil, invalid("%s takes true or false", field)
		}
		return raw, nil
	}
	return nil, invalid("%s takes no value here", field)
}
