package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// notebookMaxCellOutput bounds the rendered text of a single cell's output,
// mirroring the article's "any cell output over 10,000 chars becomes a
// pointer" rule so one dataframe dump cannot eat the read budget.
const notebookMaxCellOutput = 10_000

// notebook is the subset of the .ipynb structure the renderer needs.
type notebook struct {
	Nbformat      int            `json:"nbformat"`
	NbformatMinor int            `json:"nbformat_minor"`
	Cells         []notebookCell `json:"cells"`
}

type notebookCell struct {
	CellType string `json:"cell_type"`
	Source   any    `json:"source"` // string or []string
	Outputs  []struct {
		OutputType string         `json:"output_type"`
		Text       any            `json:"text"` // string or []string
		Name       string         `json:"name"`
		Data       map[string]any `json:"data"` // mime -> output (text, base64 image, json)
		EvalCount  any            `json:"execution_count"`
	} `json:"outputs"`
}

// renderNotebook turns a raw .ipynb file into a compact, tagged text view:
// each cell gets a markdown/code header, source is joined, and outputs are
// collapsed — text outputs over the cap become a pointer, images become a
// one-line note instead of a base64 blob. Returns ok=false when the content
// is not a parseable notebook.
func renderNotebook(raw []byte) (string, bool) {
	var nb notebook
	if err := json.Unmarshal(raw, &nb); err != nil {
		return "", false
	}
	if nb.Nbformat < 4 && len(nb.Cells) == 0 {
		// Not a notebook we recognize (no nbformat and no cells).
		return "", false
	}
	var b strings.Builder
	for i, cell := range nb.Cells {
		source := joinLines(cell.Source)
		switch cell.CellType {
		case "markdown":
			if strings.TrimSpace(source) == "" {
				continue
			}
			fmt.Fprintf(&b, "## markdown cell %d\n%s\n", i+1, strings.TrimRight(source, "\n"))
		case "code":
			fmt.Fprintf(&b, "## code cell %d\n", i+1)
			if strings.TrimSpace(source) != "" {
				fmt.Fprintf(&b, "```python\n%s\n```\n", strings.TrimRight(source, "\n"))
			}
			renderOutputs(&b, cell.Outputs)
		default:
			fmt.Fprintf(&b, "## %s cell %d\n%s\n", cell.CellType, i+1, strings.TrimRight(source, "\n"))
		}
	}
	return strings.TrimRight(b.String(), "\n"), true
}

func renderOutputs(b *strings.Builder, outputs []struct {
	OutputType string         `json:"output_type"`
	Text       any            `json:"text"`
	Name       string         `json:"name"`
	Data       map[string]any `json:"data"`
	EvalCount  any            `json:"execution_count"`
}) {
	for _, out := range outputs {
		switch out.OutputType {
		case "stream":
			if t := joinLines(out.Text); strings.TrimSpace(t) != "" {
				fmt.Fprintf(b, "output: %s\n", capCellOutput(t))
			}
		case "execute_result", "display_data":
			// Render text/plain when present; images become a note so the
			// base64 payload never reaches context.
			if txt, ok := out.Data["text/plain"]; ok {
				fmt.Fprintf(b, "output: %s\n", capCellOutput(joinLines(txt)))
			}
			if hasImage(out.Data) {
				fmt.Fprintf(b, "output: [image/plot omitted — see the notebook in a viewer]\n")
			}
		case "error":
			if t := joinLines(out.Text); t != "" {
				fmt.Fprintf(b, "error: %s\n", capCellOutput(t))
			}
		}
	}
}

func hasImage(data map[string]any) bool {
	for mime := range data {
		if strings.HasPrefix(mime, "image/") {
			return true
		}
	}
	return false
}

// capCellOutput clamps a cell's rendered output to notebookMaxCellOutput
// chars and appends a pointer to the rest.
func capCellOutput(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= notebookMaxCellOutput {
		return s
	}
	cut := notebookMaxCellOutput
	for cut > 0 && s[cut-1] != ' ' && s[cut-1] != '\n' {
		cut--
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n… <%d more chars of output omitted>", len(s)-cut)
}

// joinLines flattens a JSON string or []string (ipynb stores source/text as
// either).
func joinLines(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, part := range t {
			if s, ok := part.(string); ok {
				sb.WriteString(s)
			}
		}
		return sb.String()
	case []string:
		return strings.Join(t, "")
	}
	return ""
}
