package mux

import (
	"github.com/sandgorgon/tui/cell"
	"github.com/sandgorgon/tui/input"
	"github.com/sandgorgon/tui/tui"
)

// flatFocusable is a single-line, border-free alternative to
// tui.Focusable. tui.Focusable always reserves a 1-cell border on every
// side around its child (a deliberate, documented v1 placeholder — see
// tui/focus.go), which needs at least 2 rows of height before the
// child has any room to paint at all, and 3 to show one line of text
// with the border still visible. This package's control-strip buttons
// and pane title bars are each only layout.Length(1) tall by design (a
// compact single-line toolbar/title, not a bordered button) — wrapping
// them in tui.Focusable was silently clipping their label to nothing.
//
// This keeps Tab/Shift-Tab focusability and click/Enter activation,
// using a background-filled bar (see Model.barStyle) rather than a
// border to show focus — Paint fills the whole cell with style(), not
// just the label's own width, which is what makes it read as a
// continuous bar instead of colored text floating on the terminal's
// own background. fill is normally ' ' (every control-strip button,
// and a minimized pane's title bar); a pane's own title bar uses '─'
// instead when expanded, since it doubles as a bordered pane's top
// border line — see paneNode, which is the only caller that varies
// this from ' '.
//
// baseStyle, when labelOnlyHighlight is true, scopes the *highlighted*
// style to just the label's own cells: past the label, the fill always
// uses baseStyle instead of whatever style(focused) resolves to. This
// has to be a plain cell.Style, not another call through style(false):
// a pane's title bar style callback is `m.titleStyle(p, focused ||
// m.paneHasFocus(id))` (see paneNode) — the OR means style(false)
// returns the *same* highlighted result as style(true) whenever
// paneHasFocus(id) is true (this widget's own Tab-focus is only one of
// two ORed inputs), so it can't be used to recover "the base look" once
// the pane already has focus some other way. baseStyle sidesteps that
// by having the caller hand over the unambiguous base value directly
// (e.g. m.titleStyle(p, false), called outside the OR'd closure).
//
// Every control-strip button passes false/zero — a button's own width
// is sized to its label already, so a full-cell highlight there reads
// as "this button," not as a highlighted bar stretching into empty
// space, and its style callback (m.controlStripStyle) has no hidden
// paneHasFocus-style state to fight anyway. A pane's title bar passes
// true: its width is layout.Fill(1) (the pane's full content width),
// independent of the label's length, so a full-cell focused fill would
// turn the whole row into one solid-colored block instead of reading
// as a highlight on the focus target specifically.
func flatFocusable(key any, label string, fill rune, labelOnlyHighlight bool, baseStyle cell.Style, style func(focused bool) cell.Style, onEvent func(input.Event) tui.Msg) tui.Node {
	return tui.Component(key, flatFocusableProps{label: label, fill: fill, labelOnlyHighlight: labelOnlyHighlight, baseStyle: baseStyle, style: style, onEvent: onEvent}, func() tui.Widget {
		return &flatFocusableWidget{}
	})
}

type flatFocusableProps struct {
	label              string
	fill               rune
	labelOnlyHighlight bool
	baseStyle          cell.Style
	style              func(focused bool) cell.Style
	onEvent            func(input.Event) tui.Msg
}

type flatFocusableWidget struct {
	label              string
	fill               rune
	labelOnlyHighlight bool
	baseStyle          cell.Style
	style              func(focused bool) cell.Style
	onEvent            func(input.Event) tui.Msg
	focused            bool
}

func (w *flatFocusableWidget) Reconcile(props any) bool {
	p := props.(flatFocusableProps)
	changed := w.label != p.label
	w.label, w.fill, w.labelOnlyHighlight, w.baseStyle, w.style, w.onEvent = p.label, p.fill, p.labelOnlyHighlight, p.baseStyle, p.style, p.onEvent
	return changed
}

func (w *flatFocusableWidget) Paint(p *cell.Painter) {
	width, height := p.Size()
	textSt := w.style(w.focused)
	fillSt := textSt
	if w.labelOnlyHighlight {
		fillSt = w.baseStyle
	}
	// Fill the whole cell first, not just the label's own width — this
	// is what makes it read as a background-colored bar (see barStyle)
	// rather than colored text floating on the terminal's own
	// background, with no seam past the label. fillSt only differs from
	// textSt (leaving a deliberate seam right past the label) for a
	// labelOnlyHighlight widget while actually highlighted — see this
	// function's doc comment for why baseStyle, not style(false), is
	// what makes that comparison possible at all.
	p.Fill(0, 0, width, height, w.fill, fillSt)
	p.Text(0, 0, w.label, textSt)
}

func (w *flatFocusableWidget) HandleEvent(e input.Event) tui.Cmd {
	if w.onEvent == nil {
		return nil
	}
	msg := w.onEvent(e)
	if msg == nil {
		return nil
	}
	return func() tui.Msg { return msg }
}

func (w *flatFocusableWidget) Focusable() bool         { return true }
func (w *flatFocusableWidget) SetFocused(focused bool) { w.focused = focused }

// barFill paints a plain background-filled Node with no label and no
// interaction — for the control strip's own trailing space, past its
// last button, so the bar's background spans the full pane width.
// tui.Text("", style) looks like it should do this but doesn't: Text's
// Paint only iterates the string's own runes, so an empty string
// touches zero cells, leaving that whole region at the terminal's
// plain background — a real, visible seam past "quit".
func barFill(style cell.Style) tui.Node {
	return tui.Component("bar-fill", barFillProps{style: style, fill: ' '}, func() tui.Widget {
		return &barFillWidget{}
	})
}

// divider is a keyed barFillWidget instance filled with fill (a rule
// glyph — see renderSplit's call site for which one, chosen by split
// direction) rather than barFill's plain space: a same-color blank bar
// with no glyph at all reads as empty space rather than a deliberate
// divider, since its background is the same theme.Border an unfocused
// title bar already uses and there's no text to distinguish it.
// barFill's own single hardcoded key is fine where it's the only
// instance in its parent Box (the control strip's trailing space);
// renderSplit's dividers between split panes need one per gap, in the
// same parent Box, so each needs its own distinct key.
func divider(key any, style cell.Style, fill rune) tui.Node {
	return tui.Component(key, barFillProps{style: style, fill: fill}, func() tui.Widget {
		return &barFillWidget{}
	})
}

type barFillProps struct {
	style cell.Style
	fill  rune
}

type barFillWidget struct {
	style cell.Style
	fill  rune
}

func (w *barFillWidget) Reconcile(props any) bool {
	p := props.(barFillProps)
	w.style, w.fill = p.style, p.fill
	return true
}

func (w *barFillWidget) Paint(p *cell.Painter) {
	width, height := p.Size()
	p.Fill(0, 0, width, height, w.fill, w.style)
}

func (w *barFillWidget) HandleEvent(input.Event) tui.Cmd { return nil }
func (w *barFillWidget) Focusable() bool                 { return false }
func (w *barFillWidget) SetFocused(bool)                 {}
