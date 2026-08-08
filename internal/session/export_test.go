package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	fixOpencode = `{
  "id": "oc-123",
  "createdAt": "2026-07-01T10:00:00Z",
  "updatedAt": 1782000000,
  "messages": [
    {"role": "system", "content": "be nice"},
    {"role": "user", "content": [{"type":"text","text":"hello opencode"},{"type":"image","url":"x"}]},
    {"role": "assistant", "content": "hi there"}
  ]
}`

	fixKilo = `{
  "id": "kilo-9",
  "model": "gpt-x",
  "timestamp": 1782000000,
  "messages": [
    {"role":"user","content":"kilo question"},
    {"role":"assistant","content":"kilo answer"}
  ]
}`

	fixCodex = `{
  "id": "cx-1",
  "title": "codex run",
  "created_at": "2026-06-01T08:30:00Z",
  "messages": [
    {"role":"user","content":"run ls"},
    {"role":"assistant","content":[{"type":"text","text":"sure"}],
     "tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},
    {"role":"user","content":"","tool_outputs":[{"tool_call_id":"call_1","content":"a.txt","is_error":false}]}
  ]
}`
)

// TestDetectKind covers the sniffing table, including the ambiguous fallback.
func TestDetectKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want SessionSource
	}{
		{"opencode", fixOpencode, SourceOpencode},
		{"kilo", fixKilo, SourceKilo},
		{"codex", fixCodex, SourceCodex},
		{"ambiguous", `{"foo":1}`, SourceAuto},
		{"no messages", `{"timestamp":123}`, SourceAuto},
		{"not json", `nope`, SourceAuto},
	} {
		if got := detectKind([]byte(tc.data)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseOpencode(t *testing.T) {
	s, err := Import(strings.NewReader(fixOpencode), SourceAuto)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if s.ID != "oc-123" || len(s.Messages) != 3 {
		t.Fatalf("id=%q msgs=%d", s.ID, len(s.Messages))
	}
	// The image part must be dropped, leaving one text block.
	if got := s.Messages[1].Content; len(got) != 1 || got[0].Text != "hello opencode" {
		t.Fatalf("content=%+v", got)
	}
	if s.Title != "hello opencode" {
		t.Fatalf("title=%q", s.Title)
	}
	if s.Created.IsZero() || s.Updated.IsZero() {
		t.Fatalf("created=%v updated=%v", s.Created, s.Updated)
	}
}

func TestParseKilo(t *testing.T) {
	s, err := Import(strings.NewReader(fixKilo), SourceKilo)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if s.ID != "kilo-9" || s.Model != "gpt-x" || len(s.Messages) != 2 {
		t.Fatalf("%+v", s)
	}
	if s.Created.IsZero() || !s.Created.Equal(s.Updated) {
		t.Fatalf("created=%v updated=%v", s.Created, s.Updated)
	}
}

func TestParseKiloSQLiteRejected(t *testing.T) {
	_, err := ParseKilo([]byte("SQLite format 3\x00binary junk"))
	if err == nil || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("want SQLite rejection, got %v", err)
	}
}

func TestParseCodexToolBlocks(t *testing.T) {
	s, err := Import(strings.NewReader(fixCodex), SourceAuto)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if s.ID != "cx-1" || s.Title != "codex run" || len(s.Messages) != 3 {
		t.Fatalf("%+v", s)
	}
	a := s.Messages[1].Content
	if len(a) != 2 || a[0].Type != "text" {
		t.Fatalf("assistant=%+v", a)
	}
	if a[1].Type != "tool_use" || a[1].ID != "call_1" || a[1].Name != "bash" {
		t.Fatalf("tool_use=%+v", a[1])
	}
	// Codex encodes arguments as a JSON string; it must land as raw JSON.
	if string(a[1].Input) != `{"cmd":"ls"}` {
		t.Fatalf("args=%s", a[1].Input)
	}
	r := s.Messages[2].Content
	if len(r) != 1 || r[0].Type != "tool_result" || r[0].ToolUseID != "call_1" || r[0].Content != "a.txt" {
		t.Fatalf("tool_result=%+v", r)
	}
}

// TestExportRoundTrip guards the regression where a rick-native export was
// re-imported by a foreign parser, silently dropping tool blocks and metadata.
func TestExportRoundTrip(t *testing.T) {
	orig, err := Import(strings.NewReader(fixCodex), SourceCodex)
	if err != nil {
		t.Fatal(err)
	}
	orig.Cwd = "/tmp/work"
	orig.Snapshots = []Snapshot{{ID: "abc", Label: "edit", MsgIdx: 1}}
	orig.Usage = Usage{Input: 5, Output: 6, CacheRead: 7, CacheWrite: 8}
	orig.Requests = []RequestUsage{
		{Index: 1, Input: 100, Output: 10, CacheRead: 90, CacheWrite: 0},
		{Index: 2, Agent: "sub", Input: 20, Output: 5, CacheRead: 80, CacheWrite: 0},
	}

	var buf bytes.Buffer
	if err := Export(orig, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\n  \"id\": \"cx-1\"") {
		t.Fatalf("default export should be compact:\n%s", buf.String())
	}

	var pretty bytes.Buffer
	if err := ExportPretty(orig, &pretty); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pretty.String(), "\n  \"id\": \"cx-1\"") {
		t.Fatalf("pretty export was not indented:\n%s", pretty.String())
	}

	back, err := Import(&buf, SourceAuto)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if back.ID != orig.ID || back.Title != orig.Title || back.Cwd != orig.Cwd {
		t.Fatalf("metadata lost: %+v", back)
	}
	if back.Usage != orig.Usage {
		t.Fatalf("usage lost: %+v", back.Usage)
	}
	if len(back.Requests) != 2 || back.Requests[0] != orig.Requests[0] || back.Requests[1] != orig.Requests[1] {
		t.Fatalf("per-request telemetry lost: %+v", back.Requests)
	}
	if len(back.Snapshots) != 1 || back.Snapshots[0].ID != "abc" {
		t.Fatalf("snapshots lost: %+v", back.Snapshots)
	}
	if len(back.Messages) != len(orig.Messages) {
		t.Fatalf("msgs=%d want %d", len(back.Messages), len(orig.Messages))
	}
	tu := back.Messages[1].Content[1]
	if tu.Type != "tool_use" || tu.Name != "bash" || tu.ID != "call_1" {
		t.Fatalf("tool_use lost: %+v", tu)
	}
	var args map[string]string
	if err := json.Unmarshal(tu.Input, &args); err != nil || args["cmd"] != "ls" {
		t.Fatalf("args mangled: %s (%v)", tu.Input, err)
	}
	if tr := back.Messages[2].Content[0]; tr.Type != "tool_result" || tr.Content != "a.txt" {
		t.Fatalf("tool_result lost: %+v", tr)
	}
}

// Foreign payloads must reach their own parser, never the native decoder.
func TestNativeDecoderRejectsForeign(t *testing.T) {
	for name, fix := range map[string]string{
		"opencode": fixOpencode, "kilo": fixKilo, "codex": fixCodex,
	} {
		if _, err := parseNative([]byte(fix)); err == nil {
			t.Errorf("%s: native decoder wrongly accepted foreign payload", name)
		}
	}
}

func TestImportErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		source SessionSource
	}{
		{"empty", "   ", SourceAuto},
		{"unknown source", "{}", "bogus"},
		{"not json", "not json", SourceAuto},
		{"no messages", `{"id":"x"}`, SourceAuto},
		{"wrong explicit source", fixOpencode, SourceKilo + "x"},
	} {
		if _, err := Import(strings.NewReader(tc.body), tc.source); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

// TestCodexArgs covers the shapes codex uses for function arguments.
func TestCodexArgs(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"json string", `"{\"a\":1}"`, `{"a":1}`},
		{"raw object", `{"a": 1}`, `{"a":1}`},
		{"indented object", "{\n  \"a\": 1\n}", `{"a":1}`},
		{"plain string", `"just text"`, `"just text"`},
		{"empty string", `""`, ``},
		{"absent", ``, ``},
	} {
		got := string(codexArgs(json.RawMessage(tc.in)))
		if got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

// TestParseTime covers RFC3339, unix seconds, unix millis and junk.
func TestParseTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string // RFC3339 UTC, or "" for the zero time
	}{
		{"rfc3339", `"2026-07-01T10:00:00Z"`, "2026-07-01T10:00:00Z"},
		{"unix seconds", `1782000000`, "2026-06-21T00:00:00Z"},
		{"unix millis", `1782000000000`, "2026-06-21T00:00:00Z"},
		{"numeric string", `"1782000000"`, "2026-06-21T00:00:00Z"},
		{"junk", `"tomorrow"`, ""},
		{"zero", `0`, ""},
		{"absent", ``, ""},
	} {
		got := parseTime(json.RawMessage(tc.in))
		if tc.want == "" {
			if !got.IsZero() {
				t.Errorf("%s: want zero time, got %v", tc.name, got)
			}
			continue
		}
		if s := got.UTC().Format(time.RFC3339); s != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, s, tc.want)
		}
	}
}

func TestExportNilSession(t *testing.T) {
	if err := Export(nil, &bytes.Buffer{}); err == nil {
		t.Fatal("want error for nil session")
	}
}
