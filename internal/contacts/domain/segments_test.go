package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

var segmentDefs = map[string]domain.AttributeDef{
	"county": {Key: "county", Label: "County", Type: domain.TypeText},
	"age":    {Key: "age", Label: "Age", Type: domain.TypeNumber},
	"joined": {Key: "joined", Label: "Joined", Type: domain.TypeDate},
	"vip":    {Key: "vip", Label: "VIP", Type: domain.TypeBoolean},
}

const groupA = "0190a0b0-0000-7000-8000-000000000001"
const groupB = "0190a0b0-0000-7000-8000-000000000002"

func TestCompileSegment_Filters(t *testing.T) {
	cases := []struct {
		name, filter, path, vars string
		groups                   []string
	}{
		{"text eq", `{"all":[{"field":"attributes.county","op":"eq","value":"Nairobi"}]}`,
			`($.attributes."county" == $v1)`, `{"v1":"Nairobi"}`, nil},
		{"text neq", `{"all":[{"field":"first_name","op":"neq","value":"Ann"}]}`,
			`($.first_name != $v1)`, `{"v1":"Ann"}`, nil},
		{"text in", `{"all":[{"field":"attributes.county","op":"in","value":["Nairobi","Mombasa"]}]}`,
			`($.attributes."county" == $v1[*])`, `{"v1":["Nairobi","Mombasa"]}`, nil},
		{"text contains, quoted literally", `{"all":[{"field":"last_name","op":"contains","value":"a.\"b\\*"}]}`,
			`($.last_name like_regex "a.\"b\\*" flag "q")`, `{}`, nil},
		{"text starts_with", `{"all":[{"field":"last_name","op":"starts_with","value":"Wa"}]}`,
			`($.last_name starts with $v1)`, `{"v1":"Wa"}`, nil},
		{"text exists", `{"all":[{"field":"attributes.county","op":"exists"}]}`,
			`exists($.attributes."county")`, `{}`, nil},
		{"email lower-cased", `{"all":[{"field":"email","op":"eq","value":"Ann@Example.COM"}]}`,
			`($.email == $v1)`, `{"v1":"ann@example.com"}`, nil},
		{"number gt", `{"all":[{"field":"attributes.age","op":"gt","value":18}]}`,
			`($.attributes."age" > $v1)`, `{"v1":18}`, nil},
		{"number lte a decimal", `{"all":[{"field":"attributes.age","op":"lte","value":-3.50}]}`,
			`($.attributes."age" <= $v1)`, `{"v1":-3.50}`, nil},
		{"number between", `{"all":[{"field":"attributes.age","op":"between","value":[18,65]}]}`,
			`($.attributes."age" >= $v1 && $.attributes."age" <= $v2)`, `{"v1":18,"v2":65}`, nil},
		{"number as text", `{"all":[{"field":"attributes.age","op":"eq","value":"42"}]}`,
			`($.attributes."age" == $v1)`, `{"v1":42}`, nil},
		{"date after", `{"all":[{"field":"attributes.joined","op":"after","value":"2026-01-01"}]}`,
			`($.attributes."joined" > $v1)`, `{"v1":"2026-01-01"}`, nil},
		{"date before", `{"all":[{"field":"created_at","op":"before","value":"2026-01-01"}]}`,
			`($.created_at < $v1)`, `{"v1":"2026-01-01"}`, nil},
		{"date between", `{"all":[{"field":"created_at","op":"between","value":["2026-01-01","2026-01-31"]}]}`,
			`($.created_at >= $v1 && $.created_at <= $v2)`, `{"v1":"2026-01-01","v2":"2026-01-31"}`, nil},
		{"boolean", `{"all":[{"field":"attributes.vip","op":"eq","value":true}]}`,
			`($.attributes."vip" == $v1)`, `{"v1":true}`, nil},
		{"group in", `{"all":[{"field":"group","op":"in","value":["` + groupA + `"]}]}`,
			`($.groups[*] == $v1[*])`, `{"v1":["` + groupA + `"]}`, []string{groupA}},
		{"group not_in, repeats once", `{"all":[{"field":"group","op":"not_in","value":["` + groupB + `","` + strings.ToUpper(groupA) + `","` + groupB + `"]}]}`,
			`!($.groups[*] == $v1[*])`, `{"v1":["` + groupB + `","` + groupA + `"]}`, []string{groupA, groupB}},
		{"the data model's example", `{"all":[{"field":"attributes.county","op":"eq","value":"Nairobi"},{"any":[{"field":"group","op":"in","value":["` + groupA + `"]},{"field":"attributes.joined","op":"after","value":"2026-01-01"}]}]}`,
			`(($.attributes."county" == $v1) && (($.groups[*] == $v2[*]) || ($.attributes."joined" > $v3)))`,
			`{"v1":"Nairobi","v2":["` + groupA + `"],"v3":"2026-01-01"}`, []string{groupA}},
		{"any at the top", `{"any":[{"field":"first_name","op":"exists"},{"field":"last_name","op":"exists"}]}`,
			`(exists($.first_name) || exists($.last_name))`, `{}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := domain.CompileSegment([]byte(c.filter), segmentDefs)
			if err != nil {
				t.Fatalf("CompileSegment: %v", err)
			}
			if !strings.HasPrefix(got.Path, "(") && !strings.HasPrefix(got.Path, "!") && !strings.HasPrefix(got.Path, "exists") {
				t.Errorf("path %q", got.Path)
			}
			if got.Path != c.path && got.Path != "("+c.path+")" {
				t.Errorf("path\n got %s\nwant %s", got.Path, c.path)
			}
			var gv, wv any
			if err := json.Unmarshal(got.Vars, &gv); err != nil {
				t.Fatalf("vars %s: %v", got.Vars, err)
			}
			_ = json.Unmarshal([]byte(c.vars), &wv)
			if fmt.Sprint(gv) != fmt.Sprint(wv) {
				t.Errorf("vars\n got %s\nwant %s", got.Vars, c.vars)
			}
			if fmt.Sprint(got.Groups) != fmt.Sprint(c.groups) {
				t.Errorf("groups %v, want %v", got.Groups, c.groups)
			}
			if got.UsesGroups != (len(c.groups) > 0) {
				t.Errorf("UsesGroups %v", got.UsesGroups)
			}
		})
	}
}

// Numbers stay decimal text, never a float.
func TestCompileSegment_NumbersKeepTheirDigits(t *testing.T) {
	got, err := domain.CompileSegment([]byte(`{"all":[{"field":"attributes.age","op":"eq","value":12345678901234567890.123456789}]}`), segmentDefs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Vars), "12345678901234567890.123456789") {
		t.Fatalf("vars %s", got.Vars)
	}
}

func TestCompileSegment_Refusals(t *testing.T) {
	cond := func(field, op, value string) string {
		if value == "" {
			return fmt.Sprintf(`{"field":%q,"op":%q}`, field, op)
		}
		return fmt.Sprintf(`{"field":%q,"op":%q,"value":%s}`, field, op, value)
	}
	many := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = cond("first_name", "exists", "")
		}
		return `{"all":[` + strings.Join(parts, ",") + `]}`
	}
	for name, filter := range map[string]string{
		"not JSON":                      `{"all":[`,
		"not an object":                 `[]`,
		"a bare condition at the top":   cond("first_name", "exists", ""),
		"an empty all":                  `{"all":[]}`,
		"all and any together":          `{"all":[` + cond("first_name", "exists", "") + `],"any":[` + cond("first_name", "exists", "") + `]}`,
		"an unknown key":                `{"all":[` + cond("first_name", "exists", "") + `],"none":[]}`,
		"an unknown key in a condition": `{"all":[{"field":"first_name","op":"exists","sql":"1=1"}]}`,
		"three levels":                  `{"all":[{"any":[{"all":[` + cond("first_name", "exists", "") + `]}]}]}`,
		"21 conditions":                 many(21),
		"an unknown field":              `{"all":[` + cond("phone", "eq", `"x"`) + `]}`,
		"an undefined attribute":        `{"all":[` + cond("attributes.town", "eq", `"x"`) + `]}`,
		"a key that isn't a key":        `{"all":[` + cond(`attributes.county\" || true || \"`, "eq", `"x"`) + `]}`,
		"an attribute with no key":      `{"all":[` + cond("attributes.", "exists", "") + `]}`,
		"a nested attribute path":       `{"all":[` + cond("attributes.county.x", "exists", "") + `]}`,
		"an unknown operator":           `{"all":[` + cond("first_name", "like", `"x"`) + `]}`,
		"gt on text":                    `{"all":[` + cond("first_name", "gt", `"x"`) + `]}`,
		"contains on a number":          `{"all":[` + cond("attributes.age", "contains", `"1"`) + `]}`,
		"before on a number":            `{"all":[` + cond("attributes.age", "before", `1`) + `]}`,
		"gt on a boolean":               `{"all":[` + cond("attributes.vip", "gt", `true`) + `]}`,
		"eq on a group":                 `{"all":[` + cond("group", "eq", `"`+groupA+`"`) + `]}`,
		"text as a number":              `{"all":[` + cond("first_name", "eq", `1`) + `]}`,
		"empty text":                    `{"all":[` + cond("first_name", "eq", `""`) + `]}`,
		"text over 255":                 `{"all":[` + cond("first_name", "eq", `"`+strings.Repeat("a", 256)+`"`) + `]}`,
		"text with a control":           `{"all":[` + cond("first_name", "contains", `"a\u0000b"`) + `]}`,
		"a number as words":             `{"all":[` + cond("attributes.age", "eq", `"ten"`) + `]}`,
		"a number with an exponent":     `{"all":[` + cond("attributes.age", "eq", `1e3`) + `]}`,
		"a date that isn't":             `{"all":[` + cond("attributes.joined", "after", `"2026-02-30"`) + `]}`,
		"a date with a time":            `{"all":[` + cond("created_at", "after", `"2026-01-01T00:00:00Z"`) + `]}`,
		"a boolean as text":             `{"all":[` + cond("attributes.vip", "eq", `"yes"`) + `]}`,
		"in with no values":             `{"all":[` + cond("first_name", "in", `[]`) + `]}`,
		"in with one not text":          `{"all":[` + cond("first_name", "in", `["a",1]`) + `]}`,
		"in with 101 values":            `{"all":[` + cond("first_name", "in", `[`+strings.TrimSuffix(strings.Repeat(`"a",`, 101), ",")+`]`) + `]}`,
		"between with one value":        `{"all":[` + cond("attributes.age", "between", `[1]`) + `]}`,
		"between backwards":             `{"all":[` + cond("attributes.age", "between", `[10,9.5]`) + `]}`,
		"dates between backwards":       `{"all":[` + cond("created_at", "between", `["2026-02-01","2026-01-01"]`) + `]}`,
		"exists with a value":           `{"all":[` + cond("first_name", "exists", `"x"`) + `]}`,
		"eq with no value":              `{"all":[` + cond("first_name", "eq", "") + `]}`,
		"eq with null":                  `{"all":[` + cond("first_name", "eq", "null") + `]}`,
		"a group that isn't an ID":      `{"all":[` + cond("group", "in", `["x"]`) + `]}`,
		"a group as one string":         `{"all":[` + cond("group", "in", `"`+groupA+`"`) + `]}`,
		"a filter over 16 KB":           `{"all":[` + cond("first_name", "eq", `"`+strings.Repeat(" ", 17<<10)+`a"`) + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := domain.CompileSegment([]byte(filter), segmentDefs)
			if !errors.Is(err, domain.ErrFilterInvalid) {
				t.Fatalf("CompileSegment = %q, %v; want ErrFilterInvalid", got.Path, err)
			}
		})
	}
}

// Exactly 20 conditions, over two levels, compile.
func TestCompileSegment_AtTheLimits(t *testing.T) {
	parts := make([]string, 10)
	for i := range parts {
		parts[i] = `{"field":"first_name","op":"exists"}`
	}
	ten := strings.Join(parts, ",")
	if _, err := domain.CompileSegment([]byte(`{"all":[`+ten+`,{"any":[`+ten+`]}]}`), segmentDefs); err != nil {
		t.Fatal(err)
	}
}

// Whatever the client sends, the path holds only our own syntax, the
// defined keys and contains' quoted literal: every other value is a
// variable. Here a value carrying path syntax ends up only in the
// variables.
func TestCompileSegment_ValuesNeverReachThePath(t *testing.T) {
	hostile := `") || true || ("`
	for _, op := range []string{"eq", "neq", "starts_with"} {
		got, err := domain.CompileSegment([]byte(fmt.Sprintf(`{"all":[{"field":"first_name","op":%q,"value":%q}]}`, op, hostile)), segmentDefs)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got.Path, "true") || !strings.Contains(string(got.Vars), `|| true ||`) {
			t.Fatalf("%s: path %s vars %s", op, got.Path, got.Vars)
		}
	}
}
