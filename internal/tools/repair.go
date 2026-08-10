package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// RepairOpts carries per-call repair configuration. It is created by the
// agent loop (execOne) and threaded through tools.Context; nil is fine for
// tests and direct callers (the universal repairs still run, no note is
// captured, and no family quirks are gated on).
type RepairOpts struct {
	// Note receives a model-readable description of any repair applied to
	// this call ("" when none). The agent appends it to the tool result so
	// the model sees what was fixed and can self-correct on the next turn.
	Note *string
	// Family gates model-conditioned quirks ("deepseek", "glm", "qwen", ...).
	// Empty enables only the universal repairs.
	Family string
}

// familyQuirks lists the extra, model-conditioned repairs per family. The
// universal repairs (null-optional, stringified-array, empty-object
// placeholder, bare-string wrap) always run for every model.
var familyQuirks = map[string][]string{
	"deepseek": {"number-string"},
	"glm":      {"number-string"},
	"qwen":     {"number-string"},
}

// FamilyForModel derives the repair-quirk family from a model id
// ("provider/model" or bare "model"). Families are the open-model lineages
// the harness mediates for; an unknown model gets "" (universal repairs
// only).
func FamilyForModel(modelID string) string {
	lower := strings.ToLower(modelID)
	switch {
	case strings.Contains(lower, "deepseek"):
		return "deepseek"
	case strings.Contains(lower, "glm"):
		return "glm"
	case strings.Contains(lower, "qwen"):
		return "qwen"
	}
	return ""
}

// strictDecode decodes JSON exactly: no unknown fields, strict types.
func strictDecode(data json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// RepairDecode is decodeArgs plus the ordered, schema-driven repair pass:
//
//  1. strict decode first — valid input is never touched;
//  2. on failure, repair flagged fields against the tool's JSON Schema
//     (null-optional omission, stringified-array parse, empty-object
//     placeholder, bare-string wrap, plus family-gated number-string
//     coercion);
//  3. re-decode strictly; success returns nil (with a note in opts when a
//     repair was applied), failure returns the strict decode error.
//
// The pass is deliberately conservative:
//   - only fields whose declared schema type disagrees with the value's JSON
//     type are candidates;
//   - string-typed fields are never unwrapped, so content that merely looks
//     like JSON (e.g. old_string "[\"a\",\"b\"]") stays intact;
//   - array repairs run in order: parse-stringified-array BEFORE
//     wrap-bare-string, so a stringified array becomes a real array, never
//     a double-wrapped "[\"[\"a\",\"b\"]\"]".
func RepairDecode(data json.RawMessage, target any, schema map[string]any, opts *RepairOpts) error {
	if strictDecode(data, target) == nil {
		return nil
	}
	repaired, note, ok := repairArgs(data, target, schema, familyOf(opts))
	if !ok {
		return strictDecode(data, target)
	}
	_ = repaired
	if opts != nil && opts.Note != nil {
		*opts.Note = note
	}
	return nil
}

func familyOf(opts *RepairOpts) string {
	if opts == nil {
		return ""
	}
	return opts.Family
}

// repairArgs attempts the ordered repair pass on a JSON object that failed
// strict decoding. It returns the repaired raw bytes, a model-readable note
// ("" when nothing was changed), and whether the repaired bytes re-parse
// strictly into target.
func repairArgs(data json.RawMessage, target any, schema map[string]any, family string) (json.RawMessage, string, bool) {
	props, required := schemaProps(schema)
	if len(props) == 0 {
		return data, "", false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data, "", false // syntax error — not repairable at this layer
	}
	requiredSet := make(map[string]bool, len(required))
	for _, k := range required {
		requiredSet[k] = true
	}
	quirks := familyQuirks[family]

	var notes []string
	for key, propVal := range props {
		raw, has := m[key]
		if !has {
			continue
		}
		prop, _ := propVal.(map[string]any)
		typ := propType(prop)
		if raw == nil {
			if requiredSet[key] {
				m[key] = zeroValue(typ)
				notes = append(notes, fmt.Sprintf("replaced null %s with %s", key, zeroName(typ)))
			} else {
				delete(m, key)
				notes = append(notes, fmt.Sprintf("omitted null %s", key))
			}
			continue
		}
		if typ == "array" {
			if repaired, value, note := repairArray(key, raw); repaired {
				m[key] = value
				notes = append(notes, note)
			}
			continue
		}
		if typ == "number" && containsStr(quirks, "number-string") {
			if s, ok := raw.(string); ok {
				if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					m[key] = n
					notes = append(notes, fmt.Sprintf("coerced %s %q to number", key, s))
				}
			}
		}
	}
	if len(notes) == 0 {
		return data, "", false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return data, "", false
	}
	if strictDecode(out, target) != nil {
		return data, "", false // repaired bytes still invalid — give up
	}
	return out, strings.Join(notes, "; "), true
}

// repairArray applies the ordered array-shape repairs to one field value.
// The order matches the harness-engineering guidance: a stringified array is
// parsed before a bare string is wrapped, so the two repairs can never
// compound into a double-wrapped array.
func repairArray(key string, raw any) (bool, any, string) {
	switch v := raw.(type) {
	case []any:
		return false, nil, "" // already an array — never touched
	case string:
		// 1. stringified array: "[\"a\",\"b\"]" -> ["a","b"]
		var arr []any
		if err := json.Unmarshal([]byte(v), &arr); err == nil {
			return true, arr, fmt.Sprintf("parsed %s from string to array", key)
		}
		// 4. bare string -> wrap in array (the parse above ran first, so a
		//    stringified array is never double-wrapped).
		return true, []any{v}, fmt.Sprintf("wrapped %s bare string in array", key)
	case map[string]any:
		if len(v) == 0 {
			// 3. empty-object placeholder where an array was expected.
			return true, []any{}, fmt.Sprintf("replaced %s empty object with array", key)
		}
		// Single object where an array was expected -> wrap it.
		return true, []any{v}, fmt.Sprintf("wrapped %s object in array", key)
	}
	return false, nil, ""
}

// schemaProps extracts the properties and required list from a tool schema.
func schemaProps(schema map[string]any) (map[string]any, []string) {
	if schema == nil {
		return nil, nil
	}
	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]string)
	return props, required
}

func propType(prop map[string]any) string {
	if prop == nil {
		return ""
	}
	t, _ := prop["type"].(string)
	return t
}

func zeroValue(typ string) any {
	switch typ {
	case "array":
		return []any{}
	case "number":
		return 0
	case "boolean":
		return false
	default:
		return ""
	}
}

func zeroName(typ string) string {
	switch typ {
	case "array":
		return "[]"
	case "number":
		return "0"
	case "boolean":
		return "false"
	default:
		return `""`
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// markdownAutoLink unwraps the degenerate `[text](http://text)` form a model
// emits when a path field is auto-linked by its markdown renderer (the
// pathString() quirk): `[notes.md](http://notes.md)` is the intended path
// `notes.md`. Only a whole-string markdown link with an http(s) target is
// unwrapped, so ordinary filenames and real markdown text pass through
// untouched.
var markdownAutoLinkRe = regexp.MustCompile(`^\[([^\]]+)\]\((https?://[^)]*)\)$`)

func unwrapMarkdownLink(s string) string {
	s = strings.TrimSpace(s)
	if m := markdownAutoLinkRe.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return s
}
