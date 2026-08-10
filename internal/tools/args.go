package tools

import (
	"bytes"
	"encoding/json"
	"strings"
)

func decodeArgs(data json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

// repairNote attaches a repair description to a result: it is stored in Meta
// (for the TUI and per-model telemetry) and appended to the model-facing
// output so the model sees exactly what was repaired and can self-correct on
// the next turn. A repaired call is never an error — it succeeded.
func repairNote(r Result, note string) Result {
	if note == "" {
		return r
	}
	if r.Meta == nil {
		r.Meta = map[string]any{}
	}
	r.Meta["repaired"] = note
	r.Output = strings.TrimRight(r.Output, "\n") + "\n<repaired: " + note + ">"
	return r
}

// RepairNoteResult is the exported form of repairNote, for tools living in
// other packages (internal/agent).
func RepairNoteResult(r Result, note string) Result { return repairNote(r, note) }

// NoteOf reads the per-call repair note threaded through Context (exported
// for agent-package tools).
func NoteOf(tc Context) string {
	if tc.Repair != nil && tc.Repair.Note != nil {
		return *tc.Repair.Note
	}
	return ""
}
