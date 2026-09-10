// Package mux is a minimizable multi-pane multiplexer built on tui's
// retained widget.Terminal, arranged in a 2D split tree (see
// splitNode), each with an always-visible title-bar row that toggles
// it between full size and collapsed-to-title-bar.
//
// Almost every pane hosts a real pty-attached process — a shell, 9sh,
// htop, vim directly, anything exec'able — via widget.Terminal,
// configured by name via package config's presets (see Spec). The one
// deliberate exception is a 9P-browsing pane (see browse.go): points a
// p9/client connection at a 9P root (typically a running 9sh's
// -listen-unix socket) instead of hosting a process, rendering a plain
// directory listing or — when a directory's shape matches 9sh's
// job-control protocol — a job table. This is "Option 4" from the
// project's own design notes: the namespace-aware capability 9sh's old
// pane package had, reproduced without 9mux ever depending on 9sh
// itself. Spec/paneState carry exactly this one more variant, nothing
// more general — everything else in this package (the split tree,
// F1-F9, minimize/zoom/resize, reconcile keying) treats a browsing
// pane exactly like any other.
//
// Minimizing is a click/Enter action on a pane's title bar, not a
// global hotkey: tui.App delivers every key to both Model.Update and
// the focused widget at once, with no way to suppress the latter — a
// hotkey pressed while a pane is focused would be forwarded straight
// into the hosted process right alongside whatever Update did with it.
// F1-F9 (jump keyboard focus straight to pane N, see paneOrder and
// Update's input.KeyEvent case) are a deliberate exception to that
// rule: they're the one case where a real global hotkey is worth the
// same forwarding-into-the-hosted-process tradeoff, chosen specifically
// because F-keys are far less likely than a plain letter/digit to
// collide with anything a real shell or its readline bindings already
// use. Panes are arranged in a layout tree (see splitNode), not a flat
// list; every node in that tree — interior split or pane leaf — keeps
// an explicit, stable key at every level: reconcile.go's key matching
// is scoped per-parent, so an unkeyed ancestor whose position (or, here,
// whose very identity across a tree restructuring) shifts would discard
// and rebuild everything beneath it — including a live Terminal's pty —
// even though the leaf node itself carried a key. See
// appendTopLevelRow's doc comment for the concrete case this guards
// against.
package mux

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sandgorgon/tui/cell"
	"github.com/sandgorgon/tui/input"
	"github.com/sandgorgon/tui/layout"
	"github.com/sandgorgon/tui/style"
	"github.com/sandgorgon/tui/tui"
	"github.com/sandgorgon/tui/widget"

	"github.com/sandgorgon/9mux/config"
)

// Spec describes a pane to create, at startup (New) or later (AddPane):
// a title plus exactly one of Argv (a pty-hosted process — the common
// case) or Browse (a 9P-browsing pane — see browse.go). Argv is a
// template, not a ready-to-run command: it's only turned into an
// *exec.Cmd by newPaneState, once a pane id actually exists, since
// spawn-time tokens ({id}, $MUX_PID — see expandSpawnTokens) need that
// id to expand. This is why Spec can't just carry a pre-built
// *exec.Cmd the way it used to — SpecFromPreset runs well before any
// pane id exists (on every control-strip render, and inside the
// digit-key handler), so nothing at that point knows what to expand
// {id} to.
type Spec struct {
	Title  string
	Argv   []string
	Browse *BrowseSpec

	// BrowseCompanion is a second, unexpanded 9P address template
	// (may itself contain {id}/$MUX_PID) that a command preset (Argv
	// set) can carry — see config.Preset.BrowseCompanion. Resolved by
	// newPaneState alongside Argv and stashed on paneState.
	// browseCompanion, which is what the title bar's 'b' key actually
	// splits off. Never set alongside Browse.
	BrowseCompanion *BrowseSpec
}

// SpecFromPreset builds a Spec from a configured preset (see package
// config) — the control strip's "+" buttons and the title-bar split
// flow's digit picker both go through this.
func SpecFromPreset(p config.Preset) Spec {
	if p.Browse != nil {
		return Spec{Title: p.Name, Browse: &BrowseSpec{Network: p.Browse.Network, Addr: p.Browse.Addr}}
	}
	s := Spec{Title: p.Name, Argv: p.Argv}
	if p.BrowseCompanion != nil {
		s.BrowseCompanion = &BrowseSpec{Network: p.BrowseCompanion.Network, Addr: p.BrowseCompanion.Addr}
	}
	return s
}

// muxPID is 9mux's own process id, substituted for $MUX_PID by
// expandSpawnTokens — lets a preset give each pane a socket path
// that's unique across multiple concurrently running 9mux instances,
// not just across panes within one of them (that part comes from
// {id}).
var muxPID = os.Getpid()

// expandSpawnTokens resolves the two spawn-time tokens a preset's argv
// may contain: {id} (this pane's own id, unique within this 9mux
// instance) and $MUX_PID (this 9mux process's pid, unique across
// instances). Plain string substitution, not shell expansion — argv is
// still exec'd directly (see newPaneState), so this needs no shell and
// can't be confused by shell quoting rules.
func expandSpawnTokens(argv []string, id int) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		a = strings.ReplaceAll(a, "{id}", strconv.Itoa(id))
		a = strings.ReplaceAll(a, "$MUX_PID", strconv.Itoa(muxPID))
		out[i] = a
	}
	return out
}

type paneState struct {
	id        int
	title     string
	minimized bool
	exited    bool
	exitErr   error

	// awaitingSplitKind is true between a beginSplitMsg (d/r pressed) and
	// whatever resolves it (a digit key picking a configured preset ->
	// splitPaneMsg, or anything else -> cancelSplitMsg) — see paneNode's
	// title-bar key handler, which is the only thing that reads or sets
	// this via those two Msg types.
	awaitingSplitKind bool
	awaitingSplitDir  layout.Direction

	// awaitingBrowseSplit is the 'b' key's own two-step flow, mirroring
	// awaitingSplitKind: true between a beginBrowseSplitMsg ('b'
	// pressed on a pane with a browseCompanion) and whatever resolves
	// it (a 'd'/'r' keypress picking the new browsing sibling's
	// direction -> splitPaneMsg, or anything else -> cancelSplitMsg,
	// which clears both this and awaitingSplitKind). No digit step
	// needed here — unlike beginSplitMsg, the sibling's spec is
	// already fixed (browseCompanion), only its direction is asked.
	awaitingBrowseSplit bool

	// Exactly one of command/browse is set — see Spec's own doc
	// comment. command drives paneNode's widget.Terminal branch;
	// browse drives its browseNode branch (see browse.go).
	command *exec.Cmd
	browse  *browseState

	// browseCompanion is a resolved (post-{id}/$MUX_PID substitution)
	// 9P address, set only alongside command when the originating
	// Spec carried a BrowseCompanion — see Spec's own doc comment.
	// The title bar's 'b' key splits it off as a sibling browsing
	// pane; nil means that key does nothing on this pane.
	browseCompanion *BrowseSpec
}

// splitNode is one node of the pane-layout tree: either a leaf
// (paneID != 0, referencing a paneState by id — the actual per-pane
// business state lives there, not here) or an interior split (paneID
// == 0, dir meaningful, children the panes/sub-splits it arranges
// along that axis). This is deliberately a separate structure from
// paneState/m.panes: paneState is "what does this pane hold", the
// tree is "where does it sit" — the two vary independently (a pane's
// own state doesn't change when it's moved to a different split).
//
// Every splitNode, leaf or interior, gets its own stable id, used as
// that node's tui.Node.Key in renderSplit. This is load-bearing, not
// cosmetic: reconcile.go's key matching is scoped per-parent (see
// tui/reconcile.go's reconcileChildren), so an interior node's own
// identity has to stay stable across frames for its *children's*
// retained state (a live Terminal's pty in particular) to survive.
// See appendTopLevelRow's doc comment for the concrete case this
// guards against.
type splitNode struct {
	id       int
	paneID   int // >0 for a leaf; 0 for an interior split
	dir      layout.Direction
	children []splitChild
}

// splitChild pairs a splitNode with its weight (layout.Fill(weight))
// within the parent split — a minimized leaf overrides this to
// layout.Length(1) at render time instead; see renderSplit.
type splitChild struct {
	weight int
	node   *splitNode
}

// Model is the multiplexer's tui.Model.
type Model struct {
	panes       []*paneState
	nextID      int
	root        *splitNode // the pane-layout tree; see splitNode's doc comment
	nextSplitID int
	presets     []config.Preset // for the control strip's "+" buttons and the split-flow digit picker
	theme       style.Theme     // for the control strip / title bar background bars — see barStyle

	// nextSplitDir is the direction the next control-strip "+" addition
	// splits along — see addPaneMsg's handling in Update. Alternated
	// after every use (Horizontal, Vertical, Horizontal, ...) so
	// repeated "+" clicks grow the tree in both dimensions instead of
	// only ever deepening one axis, the same 2D tiling a title-bar d/r
	// split already gives, now for "+" too.
	nextSplitDir layout.Direction

	// zoomedID is the id of the pane currently filling the whole
	// content area (0 = no zoom) — see toggleZoomMsg and childConstraint.
	// Every other pane stays mounted in the tree (never removed from
	// View()'s output), just collapsed to Length(0) — the same
	// preserve-retained-state-by-collapsing-not-removing approach
	// per-pane minimize already established, applied to every sibling
	// off the zoomed pane's path at once, deliberately not limited to
	// panes sitting along a Vertical axis the way minimize is.
	zoomedID int

	// redrawTickRunning tracks whether a redrawTickCmd chain is already
	// in flight — every pane is a live widget.Terminal, so this simply
	// runs for as long as at least one pane exists at all (no per-kind
	// gating needed, unlike 9sh's own pane package this was extracted
	// from, which only needed it for its one pty-hosted Kind). See
	// redrawTickCmd's doc comment for why this exists at all.
	redrawTickRunning bool

	// helpOpen is whether the built-in help screen (see help.go) is
	// currently shown, as a widget.Modal overlay in View() — toggled by
	// the control strip's "help" button (toggleHelpMsg) or closed from
	// inside the modal itself (closeHelpMsg; see helpWidget.HandleEvent
	// for why Esc/'?'/'q' are safe to claim there but not as a global
	// hotkey elsewhere in this package).
	helpOpen bool

	// focusedKey mirrors tui.FocusAware's SetFocusedKey — see that
	// method's doc comment for why a *string (allocated once in New,
	// like panes' own []*paneState pointers) rather than a plain string
	// field: SetFocusedKey is called directly by App.render() on
	// whatever Model App already holds, entirely outside the normal
	// "Update returns a new Model value, App replaces a.model" flow
	// every other field relies on — a plain field's mutation would be
	// silently discarded the moment SetFocusedKey returns, since every
	// Update-produced Model is its own copy. Mutating through the
	// shared pointer instead means every copy of this Model sees the
	// same underlying string.
	focusedKey *string
}

// New builds a Model seeded with the given panes. presets is used to
// build the control strip's "+" buttons and the title-bar split flow's
// digit picker (see SpecFromPreset) — pass whatever package config.Load
// returned.
//
// The theme is picked once, at startup, from $COLORFGBG (style.
// DetectAppearance's own doc comment explains why that heuristic and
// not a real query — there's no escape sequence every terminal answers
// for "what's your background color"), defaulting to Dark on anything
// unparseable. 9mux doesn't re-detect this mid-session (a user whose
// terminal theme changes while it's running would need to restart it
// to pick up the new autodetection result — not worth polling for) but
// the control strip's "theme" button (see themeButton/toggleThemeMsg)
// does let the user flip Dark/Light by hand, live, without a restart.
func New(presets []config.Preset, appearanceEnv func(string) string, specs ...Spec) Model {
	m := Model{presets: presets, theme: style.Default(style.DetectAppearance(appearanceEnv)), nextSplitDir: layout.Horizontal, focusedKey: new(string)}
	// root is created once, up front, as an (initially empty) Vertical
	// split — never replaced or re-wrapped afterward. The seed panes
	// passed in here (specs) are appended straight to it, since there's
	// nothing yet to split off of; every pane added later, whether via
	// a title-bar d/r split or a control-strip "+" (see addPaneMsg in
	// Update), goes through splitPane instead, reparenting into a fresh
	// interior split rather than appending another root sibling. This
	// is what appendTopLevelRow's doc comment is about — root's
	// identity has to be stable from the very first frame, or promoting
	// a lone leaf into a wrapping split later would change that leaf's
	// parent and discard its retained state (a live Terminal's pty
	// included).
	m.nextSplitID++
	m.root = &splitNode{id: m.nextSplitID, dir: layout.Vertical}
	for _, s := range specs {
		m = m.withNewPane(s)
	}
	// Init (called separately by the App, from this same starting
	// Model) starts the actual first redrawTickCmd whenever there's at
	// least one seed pane; set the flag here so it's already true
	// before Update ever runs, keeping the two in sync from frame one.
	m.redrawTickRunning = len(m.panes) > 0
	return m
}

type redrawTickMsg struct{}

// redrawInterval balances "pane output shows up promptly" against
// redraw overhead — see redrawTickCmd's doc comment.
const redrawInterval = 50 * time.Millisecond

// redrawTickCmd self-reschedules (see redrawTickMsg's handling in
// Update) for as long as at least one pane is mounted. widget.
// Terminal's own doc comment explains why this is needed: a hosted
// pty's output updates the widget's internal vt.Screen state
// continuously in a background goroutine, but that only becomes
// visible the next time the App happens to render a frame for any
// other reason. Without this, a pane's output from a command only
// appeared after some unrelated keypress forced a redraw (e.g. the
// *next* Enter, not the one that ran the command).
func redrawTickCmd() tui.Cmd {
	return func() tui.Msg {
		time.Sleep(redrawInterval)
		return redrawTickMsg{}
	}
}

// withTickIfNeeded starts a redrawTickCmd chain alongside cmd if none
// is already running on next — every pane needs it (all are live
// widget.Terminal instances), so unlike 9sh's own pane package this
// was extracted from, there's no per-Kind gate here. Shared by every
// pane-creation path (splitPaneMsg, both addPaneMsg branches) so they
// can't each start their own redundant chain.
func withTickIfNeeded(next Model, cmd tui.Cmd) (Model, tui.Cmd) {
	if next.redrawTickRunning {
		return next, cmd
	}
	next.redrawTickRunning = true
	if cmd == nil {
		return next, redrawTickCmd()
	}
	return next, tui.Batch(cmd, redrawTickCmd())
}

func (m Model) withNewPane(s Spec) Model {
	m.nextID++
	p := newPaneState(m.nextID, s)
	m.panes = append(m.panes, p)
	m.appendTopLevelRow(&splitNode{paneID: p.id})
	return m
}

// newPaneState builds a fresh paneState from spec — shared by
// withNewPane (top-level "+" additions) and splitPane (splitting an
// existing pane), so both construct a pane identically.
func newPaneState(id int, s Spec) *paneState {
	p := &paneState{id: id, title: s.Title}
	if s.Browse != nil {
		p.browse = &browseState{network: s.Browse.Network, addr: s.Browse.Addr}
		return p
	}
	argv := expandSpawnTokens(s.Argv, id)
	p.command = exec.Command(argv[0], argv[1:]...)
	if s.BrowseCompanion != nil {
		addr := expandSpawnTokens([]string{s.BrowseCompanion.Addr}, id)[0]
		p.browseCompanion = &BrowseSpec{Network: s.BrowseCompanion.Network, Addr: addr}
	}
	return p
}

// appendTopLevelRow adds leaf as a new full-width row at the bottom of
// the pane stack — used only for New's seed panes (specs), which have
// nothing yet to split off of. Every pane added afterward, whether via
// a title-bar d/r split or a control-strip "+" (see addPaneMsg in
// Update), goes through splitPane instead, so this stays a
// construction-time-only path, not a general "add a pane" one. Always
// appends to m.root's existing children slice, never replaces or
// re-wraps m.root itself: m.root's identity is fixed once, in New,
// specifically so this never has to "promote" an existing lone child
// into a new wrapping split node — doing so would change that child's
// parent from reconcile's point of view (even though the child keeps
// its own stable key), discarding its retained widget state, a live
// Terminal's pty included. Appending a sibling to an already-stable,
// already-keyed parent is the well-supported case instead.
func (m *Model) appendTopLevelRow(leaf *splitNode) {
	m.root.children = append(m.root.children, splitChild{weight: defaultPaneWeight, node: leaf})
}

// closePane removes id's pane entirely — from both the flat store and
// the layout tree — relying on tui's reconciler to dispose its retained
// widget state (a Terminal's live pty included) once its Node simply
// stops appearing in the tree, the same disposeTree mechanism that
// already runs for any other tree shrink. Closing the last remaining
// pane quits, the same way the quit button already does — there's no
// sensible empty-screen state to design for instead.
func (m Model) closePane(id int) (Model, tui.Cmd) {
	if m.find(id) == nil {
		return m, nil
	}
	panes := make([]*paneState, 0, len(m.panes))
	for _, p := range m.panes {
		if p.id != id {
			panes = append(panes, p)
		}
	}
	m.panes = panes
	m.root = removeLeafFromTree(m.root, id)
	if m.zoomedID == id {
		m.zoomedID = 0
	}
	if len(m.root.children) == 0 {
		return m, tui.Quit()
	}
	return m, nil
}

// removeLeafFromTree returns n with the leaf for paneID removed.
// Deliberately does *not* collapse an interior node left with only one
// (or zero) children into its own parent's slot — that would "unwrap"
// the remaining child into its grandparent's child list, changing that
// child's parent from reconcile's point of view even though the child
// keeps its own stable key, exactly appendTopLevelRow's promote-a-leaf
// risk, just in reverse. A leftover single-child (or, once nested
// splits exist, empty) interior node is harmless: Box lays out however
// many children it actually has, so a lone child still gets the whole
// available space.
func removeLeafFromTree(n *splitNode, paneID int) *splitNode {
	if n == nil {
		return nil
	}
	if n.paneID != 0 {
		if n.paneID == paneID {
			return nil
		}
		return n
	}
	kept := make([]splitChild, 0, len(n.children))
	for _, c := range n.children {
		if updated := removeLeafFromTree(c.node, paneID); updated != nil {
			kept = append(kept, splitChild{weight: c.weight, node: updated})
		}
	}
	n.children = kept
	return n
}

// splitPane replaces id's own tree slot with a new interior split
// containing id's existing pane and a fresh pane built from spec along
// dir, giving the pair equal weight. Returns the new pane too (nil if
// id wasn't found, in which case m is returned unchanged).
//
// Splitting a pane that currently holds retained widget state (a live
// Terminal's pty in particular) preserves that state across the
// reparent — see tui's own whole-tree key index fallback
// (sandgorgon/tui#3), which is what makes this safe.
func (m Model) splitPane(id int, dir layout.Direction, spec Spec) (Model, *paneState) {
	if orig := m.find(id); orig == nil {
		return m, nil
	} else {
		orig.awaitingSplitKind = false
		orig.awaitingBrowseSplit = false
	}
	m.nextID++
	newPane := newPaneState(m.nextID, spec)
	m.panes = append(m.panes, newPane)
	m.nextSplitID++
	splitLeaf(m.root, id, dir, &splitNode{paneID: newPane.id}, m.nextSplitID)
	return m, newPane
}

// splitLeaf walks n looking for the direct child whose leaf is
// targetPaneID, and if found, replaces that child's node in place with
// a new interior split (id newSplitID, direction dir) containing the
// original node and newLeaf, each starting at defaultPaneWeight —
// preserving the outer splitChild's own weight in its parent unchanged.
// Reports whether the target was found (and thus split).
func splitLeaf(n *splitNode, targetPaneID int, dir layout.Direction, newLeaf *splitNode, newSplitID int) bool {
	if n == nil || n.paneID != 0 {
		return false
	}
	for i, c := range n.children {
		if c.node.paneID == targetPaneID {
			wrapped := &splitNode{
				id:  newSplitID,
				dir: dir,
				children: []splitChild{
					{weight: defaultPaneWeight, node: c.node},
					{weight: defaultPaneWeight, node: newLeaf},
				},
			}
			n.children[i] = splitChild{weight: c.weight, node: wrapped}
			return true
		}
		if splitLeaf(c.node, targetPaneID, dir, newLeaf, newSplitID) {
			return true
		}
	}
	return false
}

// defaultPaneWeight is every new pane's starting weight — deliberately
// not 1: 1 is also resizePane's own floor (layout.Fill needs a positive
// weight to mean anything, and tui's own layout.Split floors Fill's
// weight at 1 internally too), so a pane starting at the resize floor
// would make '-' a no-op from the very first press. Starting well above
// the floor instead gives '-' real headroom before it hits that limit,
// and '+' symmetric room to grow.
const defaultPaneWeight = 4

// resizePane adjusts id's own weight within its direct parent's
// children by delta, clamped to a minimum of 1 (layout.Fill needs a
// positive weight to mean anything).
func (m Model) resizePane(id int, delta int) Model {
	adjustWeight(m.root, id, delta)
	return m
}

func adjustWeight(n *splitNode, targetPaneID int, delta int) bool {
	if n == nil || n.paneID != 0 {
		return false
	}
	for i, c := range n.children {
		if c.node.paneID == targetPaneID {
			w := c.weight + delta
			if w < 1 {
				w = 1
			}
			n.children[i].weight = w
			return true
		}
		if adjustWeight(c.node, targetPaneID, delta) {
			return true
		}
	}
	return false
}

// paneOrder returns every pane id in the same depth-first, left-to-
// right document order tui's own reconciler visits the tree in
// (matching renderSplit's traversal exactly, since both walk
// n.children in the same stored order) — this is the order Tab
// visits panes in, and so also the order F1-F9's pane numbering and
// Update's input.KeyEvent case rely on.
func (m Model) paneOrder() []int {
	var order []int
	var walk func(n *splitNode)
	walk = func(n *splitNode) {
		if n == nil {
			return
		}
		if n.paneID != 0 {
			order = append(order, n.paneID)
			return
		}
		for _, c := range n.children {
			walk(c.node)
		}
	}
	walk(m.root)
	return order
}

// addPaneTarget reports which pane a control-strip "+" should split —
// the last pane in paneOrder()'s document order, or false if there are
// none at all. "Last in document order" rather than "whatever's
// focused" is a deliberate simplification, not a placeholder for a
// better answer: tui.Model.Update has no way to learn which widget
// currently has focus. Splitting off the last pane instead needs no
// such visibility and is fully deterministic from Model's own state.
func addPaneTarget(m Model) (int, bool) {
	order := m.paneOrder()
	if len(order) == 0 {
		return 0, false
	}
	return order[len(order)-1], true
}

// otherDirection flips Horizontal<->Vertical — see Model.nextSplitDir.
func otherDirection(d layout.Direction) layout.Direction {
	if d == layout.Horizontal {
		return layout.Vertical
	}
	return layout.Horizontal
}

// otherAppearance flips Dark<->Light — see toggleThemeMsg.
func otherAppearance(a style.Appearance) style.Appearance {
	if a == style.Dark {
		return style.Light
	}
	return style.Dark
}

// fKeyPaneNumber reports the 1-indexed pane number an F-key requests
// (F1 -> 1, ... F9 -> 9), or false for any other key. Capped at F9/9
// panes on purpose: beyond that, a dedicated F-key per pane stops
// being a usable mnemonic anyway.
func fKeyPaneNumber(k input.Key) (int, bool) {
	if k >= input.KeyF1 && k <= input.KeyF9 {
		return int(k-input.KeyF1) + 1, true
	}
	return 0, false
}

// Init kicks off the first redrawTickCmd if any seed pane exists, plus
// a connectBrowseCmd for any seed pane that's a 9P-browsing pane (see
// browse.go) — the same connection kickoff splitPaneMsg/addPaneMsg
// trigger for a pane added later.
func (m Model) Init() tui.Cmd {
	cmds := make([]tui.Cmd, 0, len(m.panes)+1)
	if m.redrawTickRunning {
		cmds = append(cmds, redrawTickCmd())
	}
	for _, p := range m.panes {
		cmds = append(cmds, connectCmdForPane(p))
	}
	return tui.Batch(cmds...)
}

type toggleMinimizeMsg struct{ id int }
type closePaneMsg struct{ id int }

// beginSplitMsg starts the two-step split flow on id's title bar: dir
// is already chosen (d/r), the next keypress on that same title bar
// picks the new sibling's preset by digit (1-9, in config order) or
// cancels.
type beginSplitMsg struct {
	id  int
	dir layout.Direction
}

// beginBrowseSplitMsg starts the 'b' key's own two-step split flow on
// id's title bar (only reachable when that pane has a
// browseCompanion): the next keypress picks the new browsing
// sibling's direction ('d' or 'r') or cancels — see
// paneState.awaitingBrowseSplit.
type beginBrowseSplitMsg struct{ id int }

// cancelSplitMsg abandons an in-progress beginSplitMsg or
// beginBrowseSplitMsg without splitting — any title-bar key that
// isn't a recognized preset digit (awaitingSplitKind) or 'd'/'r'
// (awaitingBrowseSplit) produces this; it clears whichever of the two
// flows was actually active.
type cancelSplitMsg struct{ id int }

// splitPaneMsg actually performs the split: id's pane gets a new
// sibling along dir, built from spec.
type splitPaneMsg struct {
	id   int
	dir  layout.Direction
	spec Spec
}
type resizePaneMsg struct {
	id    int
	delta int
}
type paneExitedMsg struct {
	id  int
	err error
}
type addPaneMsg struct{ spec Spec }
type quitRequestedMsg struct{}
type toggleThemeMsg struct{}
type toggleHelpMsg struct{}
type closeHelpMsg struct{}

// toggleZoomMsg zooms id to fill the whole content area, or un-zooms
// it back to the normal split layout if it's already the zoomed pane
// — see Model.zoomedID.
type toggleZoomMsg struct{ id int }

// AddPane returns a Msg that adds a new pane, splitting off the last
// pane in document order (see addPaneTarget) rather than appending a
// root-level row.
func AddPane(s Spec) tui.Msg { return addPaneMsg{spec: s} }

func (m Model) Update(msg tui.Msg) (tui.Model, tui.Cmd) {
	switch mm := msg.(type) {
	case input.KeyEvent:
		// Every input.Event reaches Update via App.HandleInput's
		// unconditional Dispatch, regardless of which widget currently
		// has focus (see this file's own doc comment on why F1-F9
		// specifically are safe to treat as a real global hotkey here).
		// n is 1-indexed to match the F-key number shown in each title
		// bar's "[F#]" prefix (paneNode); paneOrder()'s Nth entry sits
		// at focus index controlStripFocusables + (N-1)*2, since every
		// pane contributes exactly two consecutive focusables (its
		// title bar, then its content) in that same document order.
		if n, ok := fKeyPaneNumber(mm.Key); ok {
			if order := m.paneOrder(); n <= len(order) {
				return m, tui.SetFocusCmd(m.controlStripFocusables() + (n-1)*2)
			}
		}
	case closePaneMsg:
		return m.closePane(mm.id)
	case beginSplitMsg:
		if p := m.find(mm.id); p != nil {
			p.awaitingSplitKind = true
			p.awaitingSplitDir = mm.dir
		}
	case beginBrowseSplitMsg:
		if p := m.find(mm.id); p != nil {
			p.awaitingBrowseSplit = true
		}
	case cancelSplitMsg:
		if p := m.find(mm.id); p != nil {
			p.awaitingSplitKind = false
			p.awaitingBrowseSplit = false
		}
	case splitPaneMsg:
		next, newPane := m.splitPane(mm.id, mm.dir, mm.spec)
		if newPane == nil {
			return next, nil
		}
		return withTickIfNeeded(next, connectCmdForPane(newPane))
	case resizePaneMsg:
		return m.resizePane(mm.id, mm.delta), nil
	case toggleMinimizeMsg:
		if p := m.find(mm.id); p != nil {
			p.minimized = !p.minimized
		}
	case paneExitedMsg:
		if p := m.find(mm.id); p != nil {
			p.exited = true
			p.exitErr = mm.err
		}
	case addPaneMsg:
		// Splits the last pane in document order rather than appending
		// another root-level row — see addPaneTarget's doc comment.
		// Falls back to withNewPane only if there's truly no existing
		// pane to split off of, which shouldn't happen in practice (New
		// always seeds at least one) but keeps this total rather than
		// silently dropping the pane.
		target, ok := addPaneTarget(m)
		if !ok {
			next := m.withNewPane(mm.spec)
			return withTickIfNeeded(next, connectCmdForPane(next.panes[len(next.panes)-1]))
		}
		dir := m.nextSplitDir
		next, newPane := m.splitPane(target, dir, mm.spec)
		next.nextSplitDir = otherDirection(dir)
		if newPane == nil {
			return next, nil
		}
		return withTickIfNeeded(next, connectCmdForPane(newPane))
	case quitRequestedMsg:
		return m, tui.Quit()
	case toggleThemeMsg:
		m.theme = style.Default(otherAppearance(m.theme.Appearance))
	case toggleHelpMsg:
		m.helpOpen = !m.helpOpen
	case closeHelpMsg:
		m.helpOpen = false
	case toggleZoomMsg:
		if m.zoomedID == mm.id {
			m.zoomedID = 0
		} else if m.find(mm.id) != nil {
			m.zoomedID = mm.id
		}
	case redrawTickMsg:
		// redrawTickRunning keeps this to a single chain at a time;
		// reschedule while any pane still exists, or clear the flag and
		// let it die so a pane added later starts a fresh chain instead
		// of finding one it thinks is still running.
		if len(m.panes) > 0 {
			return m, redrawTickCmd()
		}
		m.redrawTickRunning = false
		return m, nil
	}
	if next, cmd, handled := m.handleBrowseMsg(msg); handled {
		return next, cmd
	}
	return m, nil
}

func (m Model) find(id int) *paneState {
	for _, p := range m.panes {
		if p.id == id {
			return p
		}
	}
	return nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// ---- view ----

func (m Model) View() tui.Node {
	// numbers is computed once per frame from the same paneOrder() Update
	// relies on for F1-F9 — one source of truth for "which pane is
	// number N", not two traversals that could drift apart.
	numbers := make(map[int]int, len(m.panes))
	for i, id := range m.paneOrder() {
		numbers[id] = i + 1
	}
	return tui.Box(layout.Vertical,
		tui.Child(layout.Length(1), m.controlStrip()),
		// true: m.root is always a Vertical split (New's own doc comment)
		// and is never itself a leaf, so this only ever matters for
		// root's direct children — matching reality, they can minimize.
		tui.Child(layout.Fill(1), m.renderSplit(m.root, numbers, true)),
		// Length(0): a widget.Modal's own assigned Rect is never used
		// (real drawing happens via PaintOverlay, a separate full-buffer
		// pass), so this deliberately takes no space in the normal Box
		// flow; its Node just needs to exist somewhere in the tree every
		// frame for App to find it.
		tui.Child(layout.Length(0), widget.Modal(helpNode(), widget.ModalOptions{
			Theme:          m.theme,
			Title:          "Help",
			Open:           m.helpOpen,
			Width:          78,
			Height:         24,
			OnOutsideClick: func() tui.Msg { return closeHelpMsg{} },
		})),
	)
}

// renderSplit renders n recursively: a leaf becomes that pane's own
// Node (already stably keyed by paneNode itself, which now also draws
// that pane's own box-drawing frame); an interior split becomes a Box
// along n.dir, one child per entry in n.children, keyed by n's own id.
// numbers is View()'s once-per-frame pane-number map, threaded down so
// paneNode can show a "[F#]" label without each leaf re-walking the
// whole tree itself.
//
// canMinimize is whether n itself (if it turns out to be a leaf) sits
// along a Vertical split axis — minimize collapses a pane's Length(1)
// along whichever axis n.dir is; that's a sensible "show just the
// title row" for a vertical stack, but collapsing a horizontally-split
// pane's *width* to one column just garbles its title sideways with no
// readable result. So a horizontally-split pane simply can't be
// minimized at all.
func (m Model) renderSplit(n *splitNode, numbers map[int]int, canMinimize bool) tui.Node {
	if n.paneID != 0 {
		return m.paneNode(m.find(n.paneID), numbers[n.paneID], canMinimize)
	}
	childCanMinimize := n.dir == layout.Vertical
	var children []tui.BoxChild
	for _, c := range n.children {
		children = append(children, tui.Child(m.childConstraint(c, childCanMinimize), m.renderSplit(c.node, numbers, childCanMinimize)))
	}
	return tui.Box(n.dir, children...).Key(splitKey(n.id))
}

// childConstraint decides how much of the parent split's axis c gets.
// Zoom (see toggleZoomMsg) takes priority over ordinary per-pane
// minimize when a pane is zoomed: every subtree that doesn't contain
// the zoomed pane collapses to Length(1) regardless of split axis or
// its own minimized state, and the path down to the zoomed pane always
// gets Fill(1) so it actually reaches full size. Ordinary weighted/
// minimize-aware sizing only applies when nothing is zoomed.
func (m Model) childConstraint(c splitChild, canMinimize bool) layout.Constraint {
	if m.zoomedID != 0 {
		if subtreeContainsPane(c.node, m.zoomedID) {
			return layout.Fill(1)
		}
		// Length(0), not minimize's Length(1): zoom's whole point is to
		// fully hide a sibling, not leave it glanceable, and this
		// matters for more than aesthetics — see widget.Terminal.Paint's
		// width<=0||height<=0 early return, which is *why* minimize
		// (collapsing a pane's outer box to Length(1) with a title bar
		// that eats that one row, leaving content exactly 0 rows) never
		// destructively resizes a live pty. A Length(1) collapse along
		// zoom's own axis instead can leave the *other* dimension >0,
		// which misses that guard and genuinely truncates content that
		// growing back afterward can't restore unless the hosted
		// program redraws itself.
		return layout.Length(0)
	}
	if c.node.paneID != 0 && canMinimize {
		if p := m.find(c.node.paneID); p != nil && p.minimized {
			return layout.Length(1)
		}
	}
	if c.weight <= 1 {
		// '-' resize floor (see resizePane/defaultPaneWeight): plain
		// Fill(1) has no absolute floor of its own — a sibling growing
		// enough via its own '+' could still squeeze this pane arbitrarily
		// small, well past readable. layout.Min(paneMinCells) behaves
		// identically to Fill(1) for the proportional-sharing part, so
		// this is a seamless transition, not a jump — it just adds the
		// hard floor Fill never had. Below paneMinCells the answer is
		// minimize (Length(1), title only), not more '-' presses.
		return layout.Min(paneMinCells)
	}
	return layout.Fill(c.weight)
}

// paneMinCells is '-' resize's absolute floor, in cells, along
// whichever axis is being squeezed — see childConstraint. An expanded
// pane's own frame (see paneNode) always spends 1 cell on a border/
// title row or column at each end, so the smallest size that still
// shows any actual content is 1 (top border/title) + 1 (content) + 1
// (bottom border) along a Vertical squeeze, or symmetrically along a
// Horizontal one.
const paneMinCells = 3

// subtreeContainsPane reports whether paneID appears anywhere in n's
// subtree — see childConstraint.
func subtreeContainsPane(n *splitNode, paneID int) bool {
	if n == nil {
		return false
	}
	if n.paneID != 0 {
		return n.paneID == paneID
	}
	for _, c := range n.children {
		if subtreeContainsPane(c.node, paneID) {
			return true
		}
	}
	return false
}

func splitKey(id int) string { return fmt.Sprintf("split-%d", id) }

// controlStripFocusables is how many Tab-focusable widgets
// controlStrip contributes, ahead of any pane, in Tab order — one per
// configured preset, plus help/theme/quit. Used by InitialFocusAdvances
// and the F1-F9 focus-jump math, since tui ties Tab order to document/
// paint order with no independent override.
func (m Model) controlStripFocusables() int {
	return len(m.presets) + 3 // + help, theme, quit
}

func (m Model) controlStrip() tui.Node {
	var children []tui.BoxChild
	for _, p := range m.presets {
		label := "+ " + p.Name
		children = append(children, tui.Child(layout.Length(len(label)+2), m.addPaneButton(label, SpecFromPreset(p))))
	}
	children = append(children,
		tui.Child(layout.Length(8), m.helpButton()),
		tui.Child(layout.Length(9), m.themeButton()),
		tui.Child(layout.Length(8), m.quitButton()),
		// barFill (not tui.Text("", ...), which paints nothing at all for
		// an empty string) carries the same background past the last
		// button, so the strip reads as one continuous bar across the
		// pane's full width, not just up to "quit".
		tui.Child(layout.Fill(1), barFill(m.controlStripStyle(false))),
	)
	return tui.Box(layout.Horizontal, children...).Key("control-strip")
}

// barStyle is the background-filled style for a pane's own title bar:
// theme.Border (a muted, structural color) unfocused, theme.Focus when
// focused. Foreground is left at the terminal's own default rather
// than an explicit theme color, so text stays legible regardless of
// exactly which accent the terminal renders theme.Border/Focus as.
func (m Model) barStyle(focused bool) cell.Style {
	bg := m.theme.Border
	if focused {
		bg = m.theme.Focus
	}
	return cell.Style{Bg: bg, Attr: cell.AttrBold}
}

// controlStripStyle is the control strip's own bar color — deliberately
// distinct from both barStyle/theme.Border (an unfocused pane title/
// border) and theme.Focus (a focused one), so the always-visible
// top-level toolbar reads as a different, more prominent layer of
// chrome than any pane title, focused or not.
func (m Model) controlStripStyle(focused bool) cell.Style {
	st := cell.Style{Bg: m.theme.Secondary, Attr: cell.AttrBold}
	if focused {
		st.Attr |= cell.AttrReverse
	}
	return st
}

// InitialFocusAdvances is how many synthetic Tab presses cmd/9mux's
// main should feed a freshly constructed tui.App (via HandleInput,
// before Run ever reads real input) so the shell starts with keyboard
// focus on the first pane's actual content instead of, as tui.App's
// zero-value focusIdx would otherwise leave it, the control strip's
// first button. Assumes exactly one pane at startup.
func (m Model) InitialFocusAdvances() int {
	return m.controlStripFocusables() + 1 // + the first pane's own title bar
}

func (m Model) addPaneButton(label string, spec Spec) tui.Node {
	return flatFocusable("btn-"+label, " "+label+" ", ' ', false, cell.Style{}, m.controlStripStyle,
		func(e input.Event) tui.Msg {
			if !clicked(e) {
				return nil
			}
			return addPaneMsg{spec: spec}
		})
}

func (m Model) helpButton() tui.Node {
	return flatFocusable("help-btn", " help ", ' ', false, cell.Style{}, m.controlStripStyle,
		func(e input.Event) tui.Msg {
			if !clicked(e) {
				return nil
			}
			return toggleHelpMsg{}
		})
}

func (m Model) themeButton() tui.Node {
	return flatFocusable("theme-btn", " theme ", ' ', false, cell.Style{}, m.controlStripStyle,
		func(e input.Event) tui.Msg {
			if !clicked(e) {
				return nil
			}
			return toggleThemeMsg{}
		})
}

func (m Model) quitButton() tui.Node {
	return flatFocusable("quit-btn", " quit ", ' ', false, cell.Style{}, m.controlStripStyle,
		func(e input.Event) tui.Msg {
			if !clicked(e) {
				return nil
			}
			return quitRequestedMsg{}
		})
}

// clicked reports whether e is the "activate" gesture for a button-like
// control: Enter/Space from the keyboard, or a plain (non-drag) left
// click.
func clicked(e input.Event) bool {
	switch ev := e.(type) {
	case input.KeyEvent:
		return ev.Key == input.KeyEnter || ev.Rune == ' '
	case input.MouseEvent:
		return ev.Button == input.MouseLeft && !ev.Drag
	}
	return false
}

func (m Model) paneNode(p *paneState, number int, canMinimize bool) tui.Node {
	id := p.id

	chevron := "  "
	if canMinimize {
		chevron = "▾ "
		if p.minimized {
			chevron = "▸ "
		}
	}
	label := chevron + p.title
	switch {
	case p.awaitingSplitKind:
		label += "  split: " + presetHint(m.presets) + " (else cancel)"
	case p.awaitingBrowseSplit:
		label += "  browse split: d/r (else cancel)"
	default:
		hint := "x/d/r/z/+/-"
		if p.browseCompanion != nil {
			hint += "/b"
		}
		label += "  (" + hint + ")"
	}
	if number >= 1 && number <= 9 {
		label = fmt.Sprintf("[F%d] ", number) + label
	}
	if m.zoomedID == id {
		label += " [zoomed]"
	}
	if p.exited {
		label += " (exited)"
	}
	if p.browse != nil && p.browse.killMsg != "" {
		label += "  " + p.browse.killMsg
	}
	collapsed := canMinimize && p.minimized
	titleFill := ' '
	if !collapsed {
		titleFill = '─'
	}
	titleBar := flatFocusable(paneKey(id, "title"), label, titleFill, true, m.titleStyle(p, false),
		func(focused bool) cell.Style { return m.titleStyle(p, focused || m.paneHasFocus(id)) },
		func(e input.Event) tui.Msg {
			if ke, ok := e.(input.KeyEvent); ok {
				if p.awaitingSplitKind {
					if preset, ok := presetForDigit(ke.Rune, m.presets); ok {
						return splitPaneMsg{id: id, dir: p.awaitingSplitDir, spec: SpecFromPreset(preset)}
					}
					return cancelSplitMsg{id: id}
				}
				if p.awaitingBrowseSplit {
					spec := Spec{Title: p.title + " (browse)", Browse: p.browseCompanion}
					switch ke.Rune {
					case 'd':
						return splitPaneMsg{id: id, dir: layout.Vertical, spec: spec}
					case 'r':
						return splitPaneMsg{id: id, dir: layout.Horizontal, spec: spec}
					}
					return cancelSplitMsg{id: id}
				}
				switch ke.Rune {
				case 'x':
					return closePaneMsg{id: id}
				case 'd':
					return beginSplitMsg{id: id, dir: layout.Vertical}
				case 'r':
					return beginSplitMsg{id: id, dir: layout.Horizontal}
				case 'z':
					return toggleZoomMsg{id: id}
				case 'b':
					if p.browseCompanion != nil {
						return beginBrowseSplitMsg{id: id}
					}
				case '+', '=':
					return resizePaneMsg{id: id, delta: 1}
				case '-':
					return resizePaneMsg{id: id, delta: -1}
				}
			}
			if !canMinimize || !clicked(e) {
				return nil
			}
			return toggleMinimizeMsg{id: id}
		})

	var content tui.Node
	if p.browse != nil {
		content = browseNode(p)
	} else {
		content = widget.Terminal(widget.TerminalOptions{
			Command: p.command,
			OnExit:  func(err error) tui.Msg { return paneExitedMsg{id: id, err: err} },
			// Every pty-hosted pane needs tab-completion to work, so Tab
			// must reach it rather than being intercepted for focus
			// navigation. ReleaseKey is left at its default (Ctrl+\), the
			// way out to Tab-navigate title bars/buttons again.
			WantsRawTab: true,
			// Themes the "[scrollback N/M]" indicator Terminal draws
			// while scrolled back, so it matches the rest of 9mux's
			// chrome instead of rendering unstyled.
			Theme: m.theme,
		}).Key(paneKey(id, "term"))
	}

	if collapsed {
		return tui.Box(layout.Vertical,
			tui.Child(layout.Length(1), titleBar),
			tui.Child(layout.Fill(1), content),
		).Key(paneKey(id, "box"))
	}

	borderStyle := cell.Style{Bg: m.theme.Border}
	corner := func(part string, r rune) tui.Node { return divider(paneKey(id, part), borderStyle, r) }
	return tui.Box(layout.Vertical,
		tui.Child(layout.Length(1), tui.Box(layout.Horizontal,
			tui.Child(layout.Length(1), corner("tl", '┌')),
			tui.Child(layout.Fill(1), titleBar),
			tui.Child(layout.Length(1), corner("tr", '┐')),
		).Key(paneKey(id, "topborder"))),
		tui.Child(layout.Fill(1), tui.Box(layout.Horizontal,
			tui.Child(layout.Length(1), divider(paneKey(id, "left"), borderStyle, '│')),
			tui.Child(layout.Fill(1), content),
			tui.Child(layout.Length(1), divider(paneKey(id, "right"), borderStyle, '│')),
		).Key(paneKey(id, "middle"))),
		tui.Child(layout.Length(1), tui.Box(layout.Horizontal,
			tui.Child(layout.Length(1), corner("bl", '└')),
			tui.Child(layout.Fill(1), divider(paneKey(id, "bottom"), borderStyle, '─')),
			tui.Child(layout.Length(1), corner("br", '┘')),
		).Key(paneKey(id, "botborder"))),
	).Key(paneKey(id, "box"))
}

func paneKey(id int, part string) string {
	return fmt.Sprintf("pane-%d-%s", id, part)
}

// presetForDigit maps a title bar's second split-flow keypress ('1'-'9')
// to the configured preset at that 1-indexed position — digits, not
// per-preset mnemonic letters, since presets are an arbitrary,
// user-configured list (unlike 9sh's own pane package this was
// extracted from, which had a fixed 5-Kind enum with fixed letters).
func presetForDigit(r rune, presets []config.Preset) (config.Preset, bool) {
	if r < '1' || r > '9' {
		return config.Preset{}, false
	}
	idx := int(r - '1')
	if idx >= len(presets) {
		return config.Preset{}, false
	}
	return presets[idx], true
}

// presetHint renders the split flow's inline "1=shell 2=kyu ..." hint
// from presets, in the same order presetForDigit indexes them.
func presetHint(presets []config.Preset) string {
	parts := make([]string, len(presets))
	for i, p := range presets {
		parts[i] = fmt.Sprintf("%d=%s", i+1, p.Name)
	}
	return strings.Join(parts, " ")
}

func (m Model) titleStyle(p *paneState, focused bool) cell.Style {
	if p.exited {
		return cell.Style{Bg: m.theme.Error, Attr: cell.AttrBold}
	}
	return m.barStyle(focused)
}

// SetFocusedKey implements tui.FocusAware (tui v0.5.0+): App.render()
// calls this immediately before View() with the Node.Key of whichever
// focusable widget currently holds keyboard focus (nil if none/unkeyed,
// or focus is inside an active FocusScope). Stored through focusedKey's
// shared pointer — see that field's own doc comment for why a plain
// value-receiver field write wouldn't stick.
func (m Model) SetFocusedKey(key any) {
	if m.focusedKey == nil { // only a bare Model{} bypassing New would hit this
		return
	}
	s, _ := key.(string)
	*m.focusedKey = s
}

// paneHasFocus reports whether the most recently reported FocusAware
// key (see SetFocusedKey) belongs to pane id — true while focus is on
// any of that pane's own focusables (its content included), not only
// while its title bar specifically is the literal tab-focused widget.
func (m Model) paneHasFocus(id int) bool {
	if m.focusedKey == nil {
		return false
	}
	return strings.HasPrefix(*m.focusedKey, paneKey(id, ""))
}
