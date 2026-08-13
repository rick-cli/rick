// Package usage persists cumulative token usage per model per day.
//
// The file lives at ~/.config/rick/usage.json (or %APPDATA%\rick\usage.json on
// Windows). Every agent turn appends its tokens under the active model id and
// the current local day. The on-disk shape is:
//
//	{
//	  "2026-07-28": {
//	    "anthropic/claude-sonnet-4-5-20250929": {"input": 12345, "output": 6789, "cache_read": 50000, "cache_write": 3000},
//	    "openai/gpt-5":                         {"input": 9999,  "output": 1111, "cache_read": 0,      "cache_write": 0}
//	  }
//	}
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Day is one model-day of token accounting.
type Day struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// RepairCounts is one model's cumulative tool-call repair accounting: which
// tools had calls repaired and how often, so a model regressing on a tool
// contract is detectable from data (Command Code's per-model x tool repair
// telemetry).
type RepairCounts struct {
	Tools map[string]int `json:"tools,omitempty"` // tool name -> repair count
	Total int            `json:"total"`
}

// Add merges another set of counts into r.
func (r *RepairCounts) Add(o RepairCounts) {
	for tool, n := range o.Tools {
		r.Tools[tool] += n
	}
	r.Total += o.Total
}

// Total returns every token that passed through the API for this day.
func (d Day) Total() int {
	return d.Input + d.Output + d.CacheRead + d.CacheWrite
}

// CacheHitRate returns the share of input tokens served from the provider
// prompt cache, as a percentage (0..100), matching the plan docs' definition
// (docs/cache-hit-plan-2026-08-07.md): cache_read over the whole prompt
// footprint (uncached input + cache reads + cache writes).
func (d Day) CacheHitRate() float64 {
	denom := d.Input + d.CacheRead + d.CacheWrite
	if denom <= 0 {
		return 0
	}
	return float64(d.CacheRead) * 100 / float64(denom)
}

// CacheWriteWeight is the relative billed cost of a cache-write token versus
// an uncached input token. OpenAI GPT-5.6+ bills writes at 1.25x uncached
// input, so a naive count-based hit rate overstates the savings there. DeepSeek
// reports no cache-write metric, so the weight only bites on write-reporting
// providers. Exported so callers can mirror the same economics in /stats.
const CacheWriteWeight = 1.25

// NetCostHitRate is the billed-cost-aware hit rate: the share of the prompt's
// *cost* that cache reads saved. Cache reads are the discounted bucket, cache
// writes are weighted at CacheWriteWeight × their uncached cost, and uncached
// input is the baseline — so on OpenAI GPT-5.6+ a write-heavy turn scores lower
// than the count-based CacheHitRate would suggest. The honest number to display
// when cache writes are billable.
func (d Day) NetCostHitRate() float64 {
	denom := float64(d.Input+d.CacheRead) + float64(d.CacheWrite)*CacheWriteWeight
	if denom <= 0 {
		return 0
	}
	return float64(d.CacheRead) * 100 / denom
}

// Add merges another day into d.
func (d *Day) Add(o Day) {
	d.Input += o.Input
	d.Output += o.Output
	d.CacheRead += o.CacheRead
	d.CacheWrite += o.CacheWrite
}

// Tracker is the in-memory usage store.
type Tracker struct {
	mu          sync.Mutex
	dir         string
	data        map[string]map[string]ModelUsage // date -> model -> usage
	repairs     map[string]*RepairCounts         // modelID -> repair counts
	lastPersist time.Time
	dirty       bool
}

const persistInterval = 30 * time.Second

// ModelUsage holds both the day breakdown and the running total.
type ModelUsage struct {
	Days  map[string]Day `json:"days"`  // date -> usage
	Total Day            `json:"total"` // running total across all days
}

// New opens (or creates) the usage store at dir.
func New(dir string) *Tracker {
	t := &Tracker{dir: dir, data: map[string]map[string]ModelUsage{}, repairs: map[string]*RepairCounts{}}
	_ = t.Load()
	return t
}

// Load reads the store from disk. A missing or malformed file starts empty.
// The on-disk shape is date-keyed usage with an optional "repairs" sibling;
// files written before repair telemetry existed simply have no repairs key.
func (t *Tracker) Load() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	path := t.path()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	data := map[string]map[string]ModelUsage{}
	for key, value := range top {
		if key == "repairs" {
			var repairs map[string]*RepairCounts
			if err := json.Unmarshal(value, &repairs); err != nil {
				return err
			}
			t.repairs = repairs
			continue
		}
		var models map[string]ModelUsage
		if err := json.Unmarshal(value, &models); err != nil {
			return err
		}
		data[key] = models
	}
	t.data = data
	if t.repairs == nil {
		t.repairs = map[string]*RepairCounts{}
	}
	t.dirty = false
	t.lastPersist = time.Time{}
	return nil
}

// path returns the on-disk file location.
func (t *Tracker) path() string {
	return filepath.Join(t.dir, "usage.json")
}

// Record appends one turn's tokens to the active model for today.
func (t *Tracker) Record(modelID string, in, out, cacheRead, cacheWrite int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.recordLocked(modelID, Day{
		Input: in, Output: out, CacheRead: cacheRead, CacheWrite: cacheWrite,
	})
}

// RecordRepair counts one tool-call repair for a model, keyed by tool. It is
// safe to call from any agent event loop; the counts persist on the same
// schedule as token usage.
func (t *Tracker) RecordRepair(modelID, tool string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if modelID == "" || tool == "" {
		return nil
	}
	counts := t.repairs[modelID]
	if counts == nil {
		counts = &RepairCounts{Tools: map[string]int{}}
		t.repairs[modelID] = counts
	}
	counts.Tools[tool]++
	counts.Total++
	t.dirty = true
	if t.lastPersist.IsZero() || time.Since(t.lastPersist) >= persistInterval {
		return t.persistLocked()
	}
	return nil
}

// Repairs returns the per-model repair counts, keyed by model id. The map is
// a copy so callers cannot mutate the store.
func (t *Tracker) Repairs() map[string]RepairCounts {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]RepairCounts, len(t.repairs))
	for model, counts := range t.repairs {
		cp := RepairCounts{Tools: map[string]int{}, Total: counts.Total}
		for tool, n := range counts.Tools {
			cp.Tools[tool] = n
		}
		out[model] = cp
	}
	return out
}

// RepairsForModel returns one model's repair counts, or zero values when the
// model has none.
func (t *Tracker) RepairsForModel(modelID string) RepairCounts {
	t.mu.Lock()
	defer t.mu.Unlock()
	counts, ok := t.repairs[modelID]
	if !ok {
		return RepairCounts{}
	}
	cp := RepairCounts{Tools: map[string]int{}, Total: counts.Total}
	for tool, n := range counts.Tools {
		cp.Tools[tool] = n
	}
	return cp
}

// RecordWithDay is the testable core: append tokens to a model on a specific
// local day, then persist periodically. Caller must hold t.mu.
func (t *Tracker) recordLocked(modelID string, d Day) error {
	if modelID == "" {
		return nil
	}
	date := time.Now().Format("2006-01-02")
	if d.Total() == 0 {
		return nil
	}

	if t.data[date] == nil {
		t.data[date] = map[string]ModelUsage{}
	}
	mu := t.data[date][modelID]
	if mu.Days == nil {
		mu.Days = map[string]Day{}
	}
	day := mu.Days[date]
	day.Add(d)
	mu.Days[date] = day
	mu.Total.Add(d)
	t.data[date][modelID] = mu

	t.dirty = true
	if t.lastPersist.IsZero() || time.Since(t.lastPersist) >= persistInterval {
		return t.persistLocked()
	}
	return nil
}

// Flush persists pending usage updates. It is safe to call at application
// shutdown and is a no-op when all updates are already on disk.
func (t *Tracker) Flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.dirty {
		return nil
	}
	return t.persistLocked()
}

// persistLocked flushes to disk. Caller must hold t.mu.
func (t *Tracker) persistLocked() error {
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return err
	}
	out := make(map[string]any, len(t.data)+1)
	for key, value := range t.data {
		out[key] = value
	}
	if len(t.repairs) > 0 {
		out["repairs"] = t.repairs
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.path()); err != nil {
		return err
	}
	t.dirty = false
	t.lastPersist = time.Now()
	return nil
}

// ModelDays returns every day a model was used, newest first.
func (t *Tracker) ModelDays(modelID string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []string
	for date, models := range t.data {
		if _, ok := models[modelID]; ok {
			out = append(out, date)
		}
	}
	sortStringsDesc(out)
	return out
}

// ForModel returns the per-day breakdown for one model, newest first.
func (t *Tracker) ForModel(modelID string) []DayEntry {
	t.mu.Lock()
	defer t.mu.Unlock()

	var entries []DayEntry
	for date, models := range t.data {
		if mu, ok := models[modelID]; ok {
			if day, ok := mu.Days[date]; ok {
				entries = append(entries, DayEntry{Date: date, Day: day})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Date > entries[j].Date })
	return entries
}

// DayEntry is one model-day of usage.
type DayEntry struct {
	Date string
	Day  Day
}

// Models returns every model id that has any usage or repairs, sorted.
func (t *Tracker) Models() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	seen := map[string]bool{}
	for _, models := range t.data {
		for id := range models {
			seen[id] = true
		}
	}
	for id := range t.repairs {
		seen[id] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sortStrings(out)
	return out
}

// Total returns the running total across all models and days.
func (t *Tracker) Total() Day {
	t.mu.Lock()
	defer t.mu.Unlock()

	var total Day
	for _, models := range t.data {
		for _, mu := range models {
			total.Add(mu.Total)
		}
	}
	return total
}

// GrandTotal is Total across all models for a single day.
func (t *Tracker) GrandTotal(date string) Day {
	t.mu.Lock()
	defer t.mu.Unlock()

	var total Day
	if models, ok := t.data[date]; ok {
		for _, mu := range models {
			if d, ok := mu.Days[date]; ok {
				total.Add(d)
			}
		}
	}
	return total
}

// ModelTotal returns the running total for one model.
func (t *Tracker) ModelTotal(modelID string) Day {
	t.mu.Lock()
	defer t.mu.Unlock()

	var total Day
	for _, models := range t.data {
		if mu, ok := models[modelID]; ok {
			total.Add(mu.Total)
		}
	}
	return total
}

// Days returns every day that has any usage, newest first.
func (t *Tracker) Days() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]string, 0, len(t.data))
	for d := range t.data {
		out = append(out, d)
	}
	sortStringsDesc(out)
	return out
}

// DateExists reports whether any usage was recorded on date.
func (t *Tracker) DateExists(date string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.data[date]
	return ok
}

// Path returns the on-disk file location (for display in /stats).
func (t *Tracker) Path() string {
	return t.path()
}

// Clear wipes all stored usage.
func (t *Tracker) Clear() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.data = map[string]map[string]ModelUsage{}
	t.repairs = map[string]*RepairCounts{}
	t.dirty = true
	return t.persistLocked()
}

func sortStrings(s []string) {
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
}

func sortStringsDesc(s []string) {
	sort.Slice(s, func(i, j int) bool { return s[i] > s[j] })
}
