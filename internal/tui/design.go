package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// designMode toggles the /design engineering curriculum. When active, the
// system prompt carries a design-engineering brief that tells the model what
// to look for, what to change, and what to leave alone — a policy of _when_,
// so the median answer (indigo, centered, three tiles) stops being the
// default. Modeled on CommandCode's /design skill: audit, then treat, then
// truthfully claim only what is verifiable.
//
// The curriculum is frozen per session (it sits in the stable system block),
// so toggling it mid-session deliberately invalidates the provider prompt
// cache once — the same tradeoff as switching models.

// designModeEnabled reports whether the /design curriculum is active.
func (m *Model) designModeEnabled() bool {
	return m.designMode
}

// designModeKey folds the design toggle into the system-prompt freeze key so
// enabling/disabling /design invalidates the provider cache exactly once.
func (m *Model) designModeKey() string {
	if m.designMode {
		return "design"
	}
	return ""
}

// cmdDesign handles /design. With a task it enables the curriculum and starts
// the agent; bare (or "on") it enables and prompts for the task; "off"
// disables the curriculum.
func (m *Model) cmdDesign(args string) (tea.Model, tea.Cmd) {
	args = strings.TrimSpace(args)
	verb := strings.ToLower(args)
	if verb == "off" {
		if m.designMode {
			m.designMode = false
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "/design off — engineering brief removed from the system prompt", Time: nowFn()})
		} else {
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "/design is already off", Time: nowFn()})
		}
		return m, nil
	}
	// "on" and bare both enable (if needed) and then ask for the task.
	if verb == "on" || args == "" {
		if !m.designMode {
			m.designMode = true
			m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "/design on — design engineering brief active", Time: nowFn()})
		}
		m.armInput("what should I design?", pendingDesignPrompt, "")
		return m, nil
	}
	// Any other text is the design task: enable and run.
	if !m.designMode {
		m.designMode = true
		m.appendMsg(ChatMsg{Kind: MsgSystem, Text: "/design on — design engineering brief active", Time: nowFn()})
	}
	return m.startDesignRun(args)
}

// startDesignRun starts the agent with the design task as the user message.
func (m *Model) startDesignRun(task string) (tea.Model, tea.Cmd) {
	if m.running || m.agentCh != nil {
		m.interrupt()
	}
	m.appendMsg(ChatMsg{Kind: MsgUser, Text: task, Time: nowFn()})
	m.setStatus("design: " + truncate(task, 40))
	return m, m.startAgent(task)
}

// designBrief renders the design-engineering system-prompt section. It is
// stable text (no dates, no git state), so it can live in the frozen part of
// the system prompt without hurting the provider cache.
func designBrief() string {
	return `
## Design engineering brief

You are working on a UI. Apply design engineering, not "make it pretty"
vibes. Every change must be a decision that belongs to the product, and you
must be able to say why.

### The AI-generated look — name it before you ship it
A UI reads as machine-made when several of these co-occur. Any one is
defensible; the co-occurrence is the tell. Avoid the co-occurrence:
- Tech gradient / glossy energy on everything
- Generic tech hue (#6366F1 indigo) because "software"
- Equal-weight feature-tile grids with no priority
- Accent rails / side-stripe borders pretending to organize
- Unearned blur / glassmorphism with no depth system
- Stat monuments (10x, 99%) where the product story belongs
- Icon toppers above every heading
- Bounce / elastic easing with no purpose
- Default type (Inter everywhere, whatever the stack ships)
- Center-stacked composition with no layout decision made

### Principles
1. Name the job before the pixels: what is this surface for? (monitor,
   operate, compare, configure, learn, decide, explore — pick the job)
2. Typography carries the hierarchy: choose a scale (e.g. 1.25 or 1.333) and
   a typeface that fits the product; do not inherit the stack default.
3. Color is a system, not a palette: a primary hue the product earns, neutrals
   that do the work, semantic states (error/warning/success/info) that are
   distinct. Prefer OKLCH where the stack allows it.
4. Spacing is rhythm: pick a base unit (4 or 8) and keep to it.
5. Motion has a reason: transitions guide attention; ease is borrowed from the
   product's material, never bounce/elastic by default.
6. Every control has states: idle, hover, active, focus, disabled, loading,
   error, empty, success. Design them or remove the control.
7. Hierarchy, not decoration: alignment, weight, and contrast organize the
   page before any accent appears.
8. Responsive is composition, not shrinking: re-compose across widths,
   devices, and input modes (mouse, touch, keyboard).

### Working method
1. Audit first: read the current files and name the concrete tells and
   violations you see (cite the files/lines).
2. Then treat: make the smallest change that removes the named problem.
3. Claim truthfully: only say a change is "done" if you can point at the
   rendered result or the exact code that produces it. If you only inspected,
   say "inspected".

### Explicit refusals
Refuse to ship, unless the product is crypto/gaming and neon-on-black is its
identity: gradient text on CTAs, glassmorphism by default, bounce/elastic
easing, endless identical card grids, tiny rounded-square icon boxes above
headings, indigo by reflex, decorative side-stripe borders.`
}
