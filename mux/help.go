package mux

import (
	"github.com/sandgorgon/tui/cell"
	"github.com/sandgorgon/tui/input"
	"github.com/sandgorgon/tui/tui"
)

// helpScrollStep is the mouse wheel's line-at-a-time scroll amount for
// the help screen — a wheel "click" conventionally moves a few lines,
// not a whole page (PgUp/PgDown's job).
const helpScrollStep = 3

// keybindingHelp is the built-in help screen's whole content — kept as
// plain data (not generated from the hotkey-handling code it
// documents) so it can be scanned and edited on its own; keep it in
// sync by hand whenever a binding changes elsewhere in this package
// (controlStrip, paneNode's title-bar switch). Unlike the pane package
// this was extracted from, there's only one section: 9mux has no
// kyu-specific language reference or bash/zsh mental model to
// document — those stayed with 9sh's own interactive TUI, which knows
// what it's hosting; 9mux doesn't.
var keybindingHelp = []string{
	"9mux — help",
	"",
	"PgUp/PgDown or the wheel to scroll; Esc, '?', or a click outside",
	"this box to close.",
	"",
	"Control strip (always visible, top row):",
	"  + <preset>     add a pane running that configured preset",
	"                 (see ~/.config/9mux/config)",
	"  theme          toggle light/dark",
	"  quit           quit 9mux",
	"",
	"Every pane's title bar:",
	"  x              close this pane",
	"  d              split down (then pick a preset by digit, or",
	"                 anything else to cancel)",
	"  r              split right (same digit-picker as d)",
	"  z              zoom/un-zoom this pane to fill the whole screen",
	"  b              (command presets with a paired \"<name>.browse =\"",
	"                 config line only) then d/r for direction, or",
	"                 anything else to cancel: split off a 9P-browsing",
	"                 pane pointed at this pane's own socket",
	"  + / -          resize this pane along its split axis, down to one",
	"                 visible content line — smaller than that, minimize",
	"                 instead",
	"  click/Enter    minimize/restore (only along a vertical split axis)",
	"  F1-F9          jump keyboard focus straight to pane N",
	"",
	"Inside a Terminal pane, once its content has focus: Tab reaches the",
	"hosted process directly (real completion, if it has any); Ctrl+\\",
	"releases focus back to pane navigation.",
	"",
	"Inside a 9P-browsing pane (a 'browse unix:...'/'browse tcp:...'",
	"preset — see ~/.config/9mux/config):",
	"  Up/Down/click  move the cursor",
	"  Enter          descend into a directory, or preview a file",
	"  Backspace      go up a directory (or close a file preview)",
	"  r              refresh the current listing",
	"  k              (job table only) send a kill command to the",
	"                 selected job",
	"  A job table also auto-refreshes on its own whenever a non-terminal",
	"  job finishes — no need to press r just to notice that.",
	"  A directory of day-sharded history logs (9sh's /session) renders",
	"  as an aggregated, newest-first history table instead.",
	"  Esc/Backspace  close a file preview",
	"  PgUp/PgDown/wheel  scroll a file preview",
}

// helpNode is the built-in help screen's body — see toggleHelpMsg/
// closeHelpMsg in model.go for how it opens/closes (a widget.Modal
// wrapping this, in Model.View), and keybindingHelp above for its
// content.
func helpNode() tui.Node {
	return tui.Component("help-content", struct{}{}, func() tui.Widget {
		return &helpWidget{}
	})
}

// helpWidget is a plain, read-only scrollable text viewer.
// scrollOffset is a plain top-anchored offset (0 = start of the
// document), the same convention a pager like less or man uses.
type helpWidget struct {
	scrollOffset int
	lastHeight   int
}

func (w *helpWidget) Reconcile(props any) bool { return true }

func (w *helpWidget) Paint(p *cell.Painter) {
	width, height := p.Size()
	if width <= 0 || height <= 0 {
		return
	}
	w.lastHeight = height
	maxStart := max0(len(keybindingHelp) - height)
	start := clamp(w.scrollOffset, 0, maxStart)
	end := min(start+height, len(keybindingHelp))
	for y, line := range keybindingHelp[start:end] {
		p.Text(0, y, line, cell.Style{})
	}
}

func (w *helpWidget) HandleEvent(e input.Event) tui.Cmd {
	switch ev := e.(type) {
	case input.MouseEvent:
		switch ev.Button {
		case input.MouseWheelUp:
			w.scrollOffset = max0(w.scrollOffset - helpScrollStep)
		case input.MouseWheelDown:
			w.scrollOffset += helpScrollStep
		}
	case input.KeyEvent:
		switch {
		case ev.Key == input.KeyEsc, ev.Rune == '?', ev.Rune == 'q':
			// Safe to claim these here (unlike a global hotkey elsewhere
			// in this package): widget.Modal claims focus exclusively for
			// its body while open, so nothing else could receive this key
			// instead.
			return func() tui.Msg { return closeHelpMsg{} }
		case ev.Key == input.KeyPgUp:
			w.scrollOffset = max0(w.scrollOffset - max0(w.lastHeight-1))
		case ev.Key == input.KeyPgDown:
			w.scrollOffset += max0(w.lastHeight - 1)
		}
	}
	return nil
}

func (w *helpWidget) Focusable() bool { return true }
func (w *helpWidget) SetFocused(bool) {}
