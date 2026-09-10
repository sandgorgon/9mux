package mux

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandgorgon/tui/input"
	"github.com/sandgorgon/tui/layout"
	"github.com/sandgorgon/tui/tui"

	"github.com/sandgorgon/9mux/config"
)

func skipUnlessOnPath(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH", name)
	}
}

// nilEnv is New's appearanceEnv for tests that don't care about theme
// detection — every key resolves to "", so style.DetectAppearance
// deterministically falls back to Dark, same as an unset $COLORFGBG in
// a real environment.
func nilEnv(string) string { return "" }

// testPresets is a stand-in for what package config.Load would return
// — its exact length/names don't matter to most tests (which reach the
// control strip's non-preset buttons via Model.controlStripFocusables()
// rather than a hardcoded count), but a couple of preset-picker tests
// use it directly.
var testPresets = []config.Preset{{Name: "a", Argv: []string{"true"}}, {Name: "b", Argv: []string{"true"}}}

// testSpec is a Spec a test doesn't care about the actual process
// behavior of — "true" exits immediately and is on every system's
// PATH. Tests that need real, observable pty output (a live READY
// marker, etc.) build their own Spec with a real argv instead.
func testSpec(title string) Spec {
	return Spec{Title: title, Argv: []string{"true"}}
}

func newTestModel(specs ...Spec) Model {
	return New(testPresets, nilEnv, specs...)
}

// ---- pure Model.Update/View logic — no tui.App needed ----

func TestNewSeedsPanes(t *testing.T) {
	m := newTestModel(testSpec("a"), testSpec("b"))
	if len(m.panes) != 2 {
		t.Fatalf("got %d panes, want 2", len(m.panes))
	}
	if m.panes[0].id == m.panes[1].id {
		t.Fatal("panes got the same id")
	}
}

// TestControlStripColorDistinctFromPaneTitles guards against the
// regression this package inherited from 9sh's own pane package:
// controlStripStyle used to base itself on theme.Primary, which is
// literally the same RGB value as theme.Focus in tui/style's own
// default themes — so a focused pane's title bar/top border came out
// visually identical to the always-visible control strip above it.
func TestControlStripColorDistinctFromPaneTitles(t *testing.T) {
	m := newTestModel(testSpec("a"))
	for _, csFocused := range []bool{false, true} {
		csBg := m.controlStripStyle(csFocused).Bg
		if csBg == m.theme.Border {
			t.Errorf("controlStripStyle(%v).Bg matches theme.Border (an unfocused pane title's color)", csFocused)
		}
		if csBg == m.theme.Focus {
			t.Errorf("controlStripStyle(%v).Bg matches theme.Focus (a focused pane title's color)", csFocused)
		}
	}
}

func TestToggleMinimizeFlipsState(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id
	if m.panes[0].minimized {
		t.Fatal("new pane should start expanded")
	}
	next, _ := m.Update(toggleMinimizeMsg{id: id})
	m = next.(Model)
	if !m.panes[0].minimized {
		t.Fatal("first toggle should minimize")
	}
	next, _ = m.Update(toggleMinimizeMsg{id: id})
	m = next.(Model)
	if m.panes[0].minimized {
		t.Fatal("second toggle should restore")
	}
}

func TestToggleMinimizeUnknownIDIsNoOp(t *testing.T) {
	m := newTestModel(testSpec("a"))
	next, cmd := m.Update(toggleMinimizeMsg{id: 999})
	if cmd != nil {
		t.Fatal("unknown id should not produce a Cmd")
	}
	m2 := next.(Model)
	if m2.panes[0].minimized {
		t.Fatal("unknown id toggle should not affect any real pane")
	}
}

func TestAddPaneMsgAppendsPane(t *testing.T) {
	m := newTestModel(testSpec("a"))
	next, _ := m.Update(addPaneMsg{spec: testSpec("b")})
	m = next.(Model)
	if len(m.panes) != 2 {
		t.Fatalf("got %d panes, want 2", len(m.panes))
	}
	if m.panes[1].title != "b" {
		t.Fatalf("new pane title = %q, want b", m.panes[1].title)
	}
}

// TestAddPaneMsgSplitsLastPaneInsteadOfAppendingRow guards the
// addPaneMsg behavior: a control-strip "+" splits the last pane in
// document order (see addPaneTarget) rather than appending another
// root-level row, and alternates direction each time (see
// Model.nextSplitDir) so repeated additions actually tile in two
// dimensions instead of only ever deepening root's own Vertical axis.
func TestAddPaneMsgSplitsLastPaneInsteadOfAppendingRow(t *testing.T) {
	m := newTestModel(testSpec("a"))
	idA := m.panes[0].id

	// New's own doc comment: nextSplitDir starts Horizontal, so the
	// first "+" should split A to the right.
	next, _ := m.Update(addPaneMsg{spec: testSpec("b")})
	m = next.(Model)
	idB := m.panes[1].id

	if len(m.root.children) != 1 {
		t.Fatalf("root has %d children, want 1 (still no new root-level row)", len(m.root.children))
	}
	row := m.root.children[0].node
	if row.paneID != 0 {
		t.Fatal("root's sole child should now be an interior split, not still a bare leaf")
	}
	if row.dir != layout.Horizontal {
		t.Fatalf("first + split direction = %v, want Horizontal", row.dir)
	}
	if len(row.children) != 2 || row.children[0].node.paneID != idA || row.children[1].node.paneID != idB {
		t.Fatalf("first + split children = %+v, want [%d, %d]", row.children, idA, idB)
	}

	// Second "+" should alternate to Vertical, and split off the last
	// pane in document order (B), not the root or A.
	next, _ = m.Update(addPaneMsg{spec: testSpec("c")})
	m = next.(Model)
	idC := m.panes[2].id

	if len(m.root.children) != 1 {
		t.Fatalf("root has %d children after second +, want 1", len(m.root.children))
	}
	row = m.root.children[0].node
	if row.dir != layout.Horizontal || len(row.children) != 2 {
		t.Fatalf("outer split changed shape after second +: dir=%v children=%d, want unchanged Horizontal/2", row.dir, len(row.children))
	}
	if row.children[0].node.paneID != idA {
		t.Fatalf("A should still be the outer split's first child, unmoved by the second +")
	}
	inner := row.children[1].node
	if inner.paneID != 0 {
		t.Fatal("B's slot should now be an interior split, not still a bare leaf")
	}
	if inner.dir != layout.Vertical {
		t.Fatalf("second + split direction = %v, want Vertical (alternated from the first)", inner.dir)
	}
	if len(inner.children) != 2 || inner.children[0].node.paneID != idB || inner.children[1].node.paneID != idC {
		t.Fatalf("second + split children = %+v, want [%d, %d]", inner.children, idB, idC)
	}

	if order := m.paneOrder(); len(order) != 3 || order[0] != idA || order[1] != idB || order[2] != idC {
		t.Fatalf("paneOrder() = %v, want [%d, %d, %d]", order, idA, idB, idC)
	}
}

func TestClosePaneRemovesFromPanesAndTree(t *testing.T) {
	m := newTestModel(testSpec("a"), testSpec("b"))
	closeID := m.panes[0].id
	keepID := m.panes[1].id

	next, cmd := m.Update(closePaneMsg{id: closeID})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("closing one of two panes should not produce a Cmd (only the last one quits)")
	}
	if len(m.panes) != 1 {
		t.Fatalf("got %d panes, want 1", len(m.panes))
	}
	if m.panes[0].id != keepID {
		t.Fatalf("wrong pane remained: got id %d, want %d", m.panes[0].id, keepID)
	}
	if len(m.root.children) != 1 {
		t.Fatalf("root has %d children, want 1", len(m.root.children))
	}
	if m.root.children[0].node.paneID != keepID {
		t.Fatalf("remaining tree leaf paneID = %d, want %d", m.root.children[0].node.paneID, keepID)
	}
}

func TestCloseUnknownIDIsNoOp(t *testing.T) {
	m := newTestModel(testSpec("a"))
	next, cmd := m.Update(closePaneMsg{id: 999})
	if cmd != nil {
		t.Fatal("unknown id should not produce a Cmd")
	}
	m2 := next.(Model)
	if len(m2.panes) != 1 {
		t.Fatalf("got %d panes, want 1 (unchanged)", len(m2.panes))
	}
}

func TestClosingLastPaneQuits(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id
	next, cmd := m.Update(closePaneMsg{id: id})
	if cmd == nil {
		t.Fatal("expected a Cmd when closing the last remaining pane")
	}
	if _, ok := cmd().(tui.QuitMsg); !ok {
		t.Fatalf("Cmd produced %T, want tui.QuitMsg", cmd())
	}
	m2 := next.(Model)
	if len(m2.panes) != 0 {
		t.Fatalf("got %d panes, want 0", len(m2.panes))
	}
}

// TestClosingOnePaneKeepsSiblingAlive is TestAddingSecondPaneKeepsFirstPaneAlive's
// counterpart in the other direction: removing a leaf from root's
// children must not disturb its remaining sibling's retained state.
func TestClosingOnePaneKeepsSiblingAlive(t *testing.T) {
	skipUnlessOnPath(t, "sh")
	survivor := []string{"sh", "-c", "echo SURVIVOR; read x; echo GOT:$x"}
	doomed := []string{"sh", "-c", "echo DOOMED; read x; echo GOT:$x"}
	m := newTestModel(Spec{Title: "survivor", Argv: survivor}, Spec{Title: "doomed", Argv: doomed})
	doomedID := m.panes[1].id

	app := tui.NewApp(m, 40, 16)
	defer app.Close()

	waitForText(t, app, "SURVIVOR", 3*time.Second)
	waitForText(t, app, "DOOMED", 3*time.Second)

	app.Dispatch(closePaneMsg{id: doomedID})
	forceRenders(app, 3)

	buf := app.Buffer().String()
	if strings.Contains(buf, "DOOMED") {
		t.Fatal("closed pane's content is still on screen")
	}
	if strings.Contains(buf, "failed to start") {
		t.Fatal("closing one pane discarded its sibling's retained state — the sibling's running process was killed")
	}
	if !strings.Contains(buf, "SURVIVOR") {
		t.Fatalf("surviving pane's output vanished after closing its sibling:\n%s", buf)
	}
}

// TestTitleBarXKeyClosesPane drives the real input path (app.HandleInput,
// not a synthetic Update(closePaneMsg{...}) call) to confirm 'x' actually
// closes a pane only once focus has reached its title bar.
func TestTitleBarXKeyClosesPane(t *testing.T) {
	skipUnlessOnPath(t, "sh")
	first := []string{"sh", "-c", "echo FIRSTPANE; read x"}
	second := []string{"sh", "-c", "echo SECONDPANE; read x"}
	m := newTestModel(Spec{Title: "first", Argv: first}, Spec{Title: "second", Argv: second})

	app := tui.NewApp(m, 40, 16)
	defer app.Close()

	waitForText(t, app, "FIRSTPANE", 3*time.Second)
	waitForText(t, app, "SECONDPANE", 3*time.Second)

	for range m.controlStripFocusables() {
		app.HandleInput(input.KeyEvent{Key: input.KeyTab})
	}
	for _, cmd := range app.HandleInput(input.KeyEvent{Rune: 'x'}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 2)

	buf := app.Buffer().String()
	if strings.Contains(buf, "FIRSTPANE") {
		t.Fatalf("first pane should have been closed by 'x' on its title bar:\n%s", buf)
	}
	if !strings.Contains(buf, "SECONDPANE") {
		t.Fatalf("second pane should still be visible:\n%s", buf)
	}
}

func TestSplitPaneAddsSiblingInTree(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id

	next, _ := m.Update(splitPaneMsg{id: id, dir: layout.Horizontal, spec: testSpec("kyu")})
	m = next.(Model)
	if len(m.panes) != 2 {
		t.Fatalf("got %d panes, want 2", len(m.panes))
	}

	// root wraps [original top-level row] -> now that row's own leaf
	// should have become an interior Horizontal split of [original, new].
	if len(m.root.children) != 1 {
		t.Fatalf("root has %d children, want 1 (unchanged — split happens inside the row, not at root)", len(m.root.children))
	}
	row := m.root.children[0].node
	if row.paneID != 0 {
		t.Fatal("the split row should now be an interior node, not still a bare leaf")
	}
	if row.dir != layout.Horizontal {
		t.Fatalf("split direction = %v, want Horizontal", row.dir)
	}
	if len(row.children) != 2 {
		t.Fatalf("split row has %d children, want 2", len(row.children))
	}
	if row.children[0].node.paneID != id {
		t.Fatalf("first child paneID = %d, want original pane %d", row.children[0].node.paneID, id)
	}
	if row.children[1].node.paneID != m.panes[1].id {
		t.Fatalf("second child paneID = %d, want new pane %d", row.children[1].node.paneID, m.panes[1].id)
	}
}

// TestHorizontalSplitPaintsPerPaneBorders drives a real horizontal
// split through the actual View()/renderSplit/paneNode render path and
// checks the frame buffer directly for each pane's own box-drawing
// frame — scans a content row for '│' columns: two panes side by side,
// each drawing its own left+right border, means 4.
func TestHorizontalSplitPaintsPerPaneBorders(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id

	next, _ := m.Update(splitPaneMsg{id: id, dir: layout.Horizontal, spec: testSpec("b")})
	m = next.(Model)

	app := tui.NewApp(m, 40, 10)
	defer app.Close()
	forceRenders(app, 1)

	buf := app.Buffer()
	const contentRow = 5 // well below the control strip + top-border row
	sideCols := 0
	for x := range 40 {
		c := buf.At(x, contentRow)
		if c.Style.Bg == m.theme.Border && c.Rune == '│' {
			sideCols++
		}
	}
	if sideCols != 4 {
		t.Fatalf("got %d border-styled '│' columns at row %d, want 4 (2 panes x left+right each)", sideCols, contentRow)
	}

	const topBorderRow = 1 // row 0 is the control strip
	topLeftCorners := 0
	for x := range 40 {
		c := buf.At(x, topBorderRow)
		if c.Style.Bg == m.theme.Border && c.Rune == '┌' {
			topLeftCorners++
		}
	}
	if topLeftCorners != 2 {
		t.Fatalf("got %d '┌' corners at row %d, want 2 (one per pane)", topLeftCorners, topBorderRow)
	}
}

// TestVerticalSplitPaintsPerPaneBorders is
// TestHorizontalSplitPaintsPerPaneBorders's counterpart for the other
// split axis.
func TestVerticalSplitPaintsPerPaneBorders(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id

	next, _ := m.Update(splitPaneMsg{id: id, dir: layout.Vertical, spec: testSpec("b")})
	m = next.(Model)

	app := tui.NewApp(m, 40, 14)
	defer app.Close()
	forceRenders(app, 1)

	buf := app.Buffer()
	foundRule := false
	for y := range 14 {
		if buf.At(20, y).Style.Bg == m.theme.Border && buf.At(20, y).Rune == '─' {
			foundRule = true
			break
		}
	}
	if !foundRule {
		t.Fatalf("expected at least one '─' border cell at column 20:\n%s", buf.String())
	}

	foundTL, foundBL := false, false
	for y := range 14 {
		c := buf.At(0, y)
		if c.Style.Bg != m.theme.Border {
			continue
		}
		switch c.Rune {
		case '┌':
			foundTL = true
		case '└':
			foundBL = true
		}
	}
	if !foundTL || !foundBL {
		t.Fatalf("expected both '┌' and '└' somewhere in column 0: foundTL=%v foundBL=%v", foundTL, foundBL)
	}
}

func TestSplitUnknownIDIsNoOp(t *testing.T) {
	m := newTestModel(testSpec("a"))
	next, cmd := m.Update(splitPaneMsg{id: 999, dir: layout.Vertical, spec: testSpec("b")})
	if cmd != nil {
		t.Fatal("unknown id should not produce a Cmd")
	}
	m2 := next.(Model)
	if len(m2.panes) != 1 {
		t.Fatalf("got %d panes, want 1 (unchanged)", len(m2.panes))
	}
}

func TestResizeAdjustsWeightAndClamps(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id
	next, _ := m.Update(splitPaneMsg{id: id, dir: layout.Horizontal, spec: testSpec("b")})
	m = next.(Model)
	row := m.root.children[0].node

	next, _ = m.Update(resizePaneMsg{id: id, delta: 3})
	m = next.(Model)
	row = m.root.children[0].node
	if want := defaultPaneWeight + 3; row.children[0].weight != want {
		t.Fatalf("weight after +3 = %d, want %d (started at defaultPaneWeight=%d)", row.children[0].weight, want, defaultPaneWeight)
	}

	next, _ = m.Update(resizePaneMsg{id: id, delta: -10})
	m = next.(Model)
	row = m.root.children[0].node
	if row.children[0].weight != 1 {
		t.Fatalf("weight after a large decrease = %d, want clamped to 1", row.children[0].weight)
	}
}

func TestResizeFloorSwitchesToAbsoluteMinCells(t *testing.T) {
	m := newTestModel(testSpec("a"))

	node := &splitNode{}
	if got := m.childConstraint(splitChild{weight: 2, node: node}, true); got != layout.Fill(2) {
		t.Errorf("childConstraint at weight 2 = %v, want Fill(2)", got)
	}
	if got := m.childConstraint(splitChild{weight: 1, node: node}, true); got != layout.Min(paneMinCells) {
		t.Errorf("childConstraint at weight 1 (the resize floor) = %v, want Min(%d)", got, paneMinCells)
	}
}

func TestResizingASmallerPaneBelowItsStartingWeightWorks(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id
	next, _ := m.Update(splitPaneMsg{id: id, dir: layout.Horizontal, spec: testSpec("b")})
	m = next.(Model)
	row := m.root.children[0].node
	startWeight := row.children[0].weight

	next, _ = m.Update(resizePaneMsg{id: id, delta: -1})
	m = next.(Model)
	row = m.root.children[0].node
	if row.children[0].weight >= startWeight {
		t.Fatalf("weight after one '-' = %d, want less than the starting weight %d", row.children[0].weight, startWeight)
	}
}

// TestSplitKeysOnTitleBar drives the real input path to confirm the
// two-step 'r' then digit-picker split flow actually works via
// app.HandleInput (not just direct Update calls) — the new sibling's
// own title bar should now be on screen too.
func TestSplitKeysOnTitleBar(t *testing.T) {
	m := newTestModel(testSpec("kyu"))

	app := tui.NewApp(m, 60, 16)
	defer app.Close()

	for range m.controlStripFocusables() {
		app.HandleInput(input.KeyEvent{Key: input.KeyTab})
	}
	for _, cmd := range app.HandleInput(input.KeyEvent{Rune: 'r'}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 1)
	if buf := app.Buffer().String(); !strings.Contains(buf, "1="+testPresets[0].Name) {
		t.Fatalf("expected the split-flow preset prompt after 'r':\n%s", buf)
	}
	for _, cmd := range app.HandleInput(input.KeyEvent{Rune: '1'}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 2)

	buf := app.Buffer().String()
	if n := strings.Count(buf, "x/d/r/z/+/-"); n != 2 {
		t.Fatalf("expected 2 title bars after splitting, found %d:\n%s", n, buf)
	}
}

// TestTitleBarBKeyOpensBrowseCompanion drives the real input path (not
// a synthetic Update(splitPaneMsg{...}) call) to confirm 'b' on a
// pane's title bar splits off a browsing pane pointed at that pane's
// already-resolved BrowseCompanion address, via the same two-step
// flow beginSplitMsg/splitPaneMsg use for 'd'/'r' — 'b' only starts
// it (awaitingBrowseSplit), and a following 'd' or 'r' actually
// splits, picking the sibling's direction the way the user asks.
func TestTitleBarBKeyOpensBrowseCompanion(t *testing.T) {
	s := Spec{
		Title:           "kyu",
		Argv:            []string{"true"},
		BrowseCompanion: &BrowseSpec{Network: "unix", Addr: "/tmp/9mux-test-{id}.sock"},
	}
	m := newTestModel(s)

	app := tui.NewApp(m, 100, 16)
	defer app.Close()
	forceRenders(app, 1)

	if buf := app.Buffer().String(); !strings.Contains(buf, "x/d/r/z/+/-/b") {
		t.Fatalf("expected the 'b' hint on a pane with a BrowseCompanion:\n%s", buf)
	}

	for range m.controlStripFocusables() {
		app.HandleInput(input.KeyEvent{Key: input.KeyTab})
	}
	for _, cmd := range app.HandleInput(input.KeyEvent{Rune: 'b'}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 1)

	if buf := app.Buffer().String(); !strings.Contains(buf, "browse split: d/r (else cancel)") {
		t.Fatalf("expected the browse-split prompt after 'b':\n%s", buf)
	}

	for _, cmd := range app.HandleInput(input.KeyEvent{Rune: 'r'}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 2)

	buf := app.Buffer().String()
	if n := strings.Count(buf, "x/d/r/z/+/-"); n != 2 {
		t.Fatalf("expected 2 title bars after 'b' then 'r', found %d:\n%s", n, buf)
	}
	if !strings.Contains(buf, "kyu (browse)") {
		t.Fatalf("expected the new sibling's title to be %q:\n%s", "kyu (browse)", buf)
	}
}

// TestTitleBarBKeyIsNoOpWithoutCompanion confirms 'b' does nothing on
// a pane whose Spec carried no BrowseCompanion — same "unrecognized
// key" no-op as any other key not in the title bar's switch.
func TestTitleBarBKeyIsNoOpWithoutCompanion(t *testing.T) {
	m := newTestModel(testSpec("kyu"))

	app := tui.NewApp(m, 60, 16)
	defer app.Close()

	for range m.controlStripFocusables() {
		app.HandleInput(input.KeyEvent{Key: input.KeyTab})
	}
	for _, cmd := range app.HandleInput(input.KeyEvent{Rune: 'b'}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 1)

	if buf := app.Buffer().String(); strings.Count(buf, "x/d/r/z/+/-") != 1 {
		t.Fatalf("'b' should be a no-op without a BrowseCompanion:\n%s", buf)
	}
}

// TestBrowseSplitFlowCancelsOnUnrecognizedKey mirrors
// TestSplitFlowCancelsOnUnrecognizedKey for the 'b' flow: any key
// other than 'd'/'r' during awaitingBrowseSplit abandons it via
// cancelSplitMsg rather than splitting in some default direction.
func TestBrowseSplitFlowCancelsOnUnrecognizedKey(t *testing.T) {
	s := Spec{
		Title:           "kyu",
		Argv:            []string{"true"},
		BrowseCompanion: &BrowseSpec{Network: "unix", Addr: "/tmp/9mux-test-{id}.sock"},
	}
	m := newTestModel(s)
	id := m.panes[0].id

	next, _ := m.Update(beginBrowseSplitMsg{id: id})
	m = next.(Model)
	if !m.panes[0].awaitingBrowseSplit {
		t.Fatal("expected awaitingBrowseSplit after beginBrowseSplitMsg")
	}

	next, cmd := m.Update(cancelSplitMsg{id: id})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("cancelSplitMsg should not produce a Cmd")
	}
	if m.panes[0].awaitingBrowseSplit {
		t.Fatal("expected awaitingBrowseSplit cleared after cancelSplitMsg")
	}
	if len(m.panes) != 1 {
		t.Fatalf("got %d panes, want 1 (cancel shouldn't split)", len(m.panes))
	}
}

// TestSplitFlowCancelsOnUnrecognizedKey confirms any key other than a
// valid preset digit during the split-flow's second step abandons it
// (via cancelSplitMsg) rather than splitting with some default preset.
func TestSplitFlowCancelsOnUnrecognizedKey(t *testing.T) {
	m := newTestModel(testSpec("kyu"))
	id := m.panes[0].id

	next, _ := m.Update(beginSplitMsg{id: id, dir: layout.Vertical})
	m = next.(Model)
	if !m.panes[0].awaitingSplitKind {
		t.Fatal("expected awaitingSplitKind after beginSplitMsg")
	}

	next, cmd := m.Update(cancelSplitMsg{id: id})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("cancelSplitMsg should not produce a Cmd")
	}
	if m.panes[0].awaitingSplitKind {
		t.Fatal("expected awaitingSplitKind cleared after cancelSplitMsg")
	}
	if len(m.panes) != 1 {
		t.Fatalf("got %d panes, want 1 (cancel shouldn't split)", len(m.panes))
	}
}

// TestPresetForDigitMapping is a direct unit test of presetForDigit/
// presetHint — the digit-to-preset mapping paneNode's title-bar closure
// relies on for the split flow's second keypress (the generalized
// replacement for 9sh's own pane package's fixed letter-per-Kind
// mapping, since presets are an arbitrary, user-configured list).
func TestPresetForDigitMapping(t *testing.T) {
	presets := []config.Preset{{Name: "shell"}, {Name: "kyu"}}
	for i, want := range presets {
		digit := rune('1' + i)
		got, ok := presetForDigit(digit, presets)
		if !ok || got.Name != want.Name {
			t.Fatalf("presetForDigit(%q) = (%+v, %v), want (%+v, true)", digit, got, ok, want)
		}
	}
	if _, ok := presetForDigit('3', presets); ok {
		t.Fatal("presetForDigit('3') should not be recognized with only 2 presets")
	}
	if _, ok := presetForDigit('q', presets); ok {
		t.Fatal("presetForDigit('q') should not be recognized at all")
	}
	if hint := presetHint(presets); hint != "1=shell 2=kyu" {
		t.Fatalf("presetHint = %q, want %q", hint, "1=shell 2=kyu")
	}
}

// TestFirstPaneAddedStartsRedrawTick confirms adding the very first
// pane to an empty Model starts exactly one redrawTickCmd chain — the
// generalized replacement for 9sh's own pane package's Kind-gated
// version, since every pane here is a live widget.Terminal (no Kind
// distinction left to gate on).
func TestFirstPaneAddedStartsRedrawTick(t *testing.T) {
	m := newTestModel() // no seed panes
	if m.redrawTickRunning {
		t.Fatal("an empty Model should not have a tick running yet")
	}

	next, cmd := m.Update(addPaneMsg{spec: testSpec("a")})
	m = next.(Model)
	if !m.redrawTickRunning {
		t.Fatal("redrawTickRunning should be true once a pane exists")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd starting the redraw tick")
	}
	if _, ok := cmd().(redrawTickMsg); !ok {
		t.Fatalf("expected the Cmd to eventually produce redrawTickMsg, got %T", cmd())
	}

	// A second pane must not start a second chain.
	_, cmd2 := m.Update(addPaneMsg{spec: testSpec("b")})
	if cmd2 != nil {
		t.Fatal("a second pane should not start a redundant tick chain")
	}
}

// TestRedrawTickStopsOnceNoPanesRemain confirms the tick chain
// reschedules itself while any pane is mounted and stops (clearing
// redrawTickRunning) once the last one closes, rather than ticking
// forever in the background.
func TestRedrawTickStopsOnceNoPanesRemain(t *testing.T) {
	m := newTestModel(testSpec("a"))
	m.redrawTickRunning = true

	next, cmd := m.Update(redrawTickMsg{})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected the tick to reschedule itself while a pane remains")
	}
	if !m.redrawTickRunning {
		t.Fatal("redrawTickRunning should stay true while rescheduling")
	}

	m.panes = nil // simulate every pane having closed
	next, cmd = m.Update(redrawTickMsg{})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("expected the tick to stop once no panes remain")
	}
	if m.redrawTickRunning {
		t.Fatal("redrawTickRunning should be cleared once the tick stops")
	}
}

// TestFKeyRequestsFocusAtComputedIndex is a direct Update-level test:
// with 3 panes, F2 should ask to focus the second pane's title bar.
func TestFKeyRequestsFocusAtComputedIndex(t *testing.T) {
	m := newTestModel(testSpec("a"), testSpec("b"), testSpec("c"))

	_, cmd := m.Update(input.KeyEvent{Key: input.KeyF2})
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd requesting a focus change")
	}
	fm, ok := cmd().(tui.FocusMsg)
	if !ok {
		t.Fatalf("expected tui.FocusMsg, got %T", cmd())
	}
	want := m.controlStripFocusables() + 2 // pane b's title bar
	if fm.Index != want {
		t.Fatalf("got focus index %d, want %d", fm.Index, want)
	}
}

// TestFKeyPastPaneCountIsNoop confirms F9 with only one pane open
// doesn't return a Cmd at all.
func TestFKeyPastPaneCountIsNoop(t *testing.T) {
	m := newTestModel(testSpec("a"))
	_, cmd := m.Update(input.KeyEvent{Key: input.KeyF9})
	if cmd != nil {
		t.Fatalf("expected a nil Cmd, got one that produces %v", cmd())
	}
}

// TestFKeyJumpsFocusEndToEnd drives the real input path: confirms a
// real F2 KeyEvent traveling through App.HandleInput's actual event
// pipeline still produces a Cmd yielding the right tui.FocusMsg. Real
// end-to-end confirmation that F2 moves live focus is a tmux/real-
// terminal check, not something a headless Go test can assert (see
// tui's own Run()/Dispatch split).
//
// findFocusMsg unwraps a possible tui.BatchMsg to find it: testSpec's
// panes run "true", which exits almost instantly, and since tui v0.6.1
// (App.Dispatch draining every widget's tui.PendingMsgSource on every
// Dispatch, not just the focused one's — see 9mux's own go.mod bump)
// an already-exited pane's OnExit Msg can legitimately ride along in
// the same Cmd as this F2 press's FocusMsg, batched together. That's
// correct behavior, not a regression to work around by asserting a
// single bare Cmd.
func findFocusMsg(t *testing.T, msg tui.Msg) tui.FocusMsg {
	t.Helper()
	switch m := msg.(type) {
	case tui.FocusMsg:
		return m
	case tui.BatchMsg:
		for _, c := range m {
			if c == nil {
				continue
			}
			if fm, ok := c().(tui.FocusMsg); ok {
				return fm
			}
		}
	}
	t.Fatalf("expected a tui.FocusMsg (bare or inside a tui.BatchMsg), got %T", msg)
	return tui.FocusMsg{}
}

func TestFKeyJumpsFocusEndToEnd(t *testing.T) {
	m := newTestModel(testSpec("a"), testSpec("b"))
	app := tui.NewApp(m, 80, 16)
	defer app.Close()

	cmds := app.HandleInput(input.KeyEvent{Key: input.KeyF2})
	if len(cmds) != 1 || cmds[0] == nil {
		t.Fatalf("expected exactly one non-nil Cmd, got %v", cmds)
	}
	fm := findFocusMsg(t, cmds[0]())
	want := m.controlStripFocusables() + 2 // pane b's title bar
	if fm.Index != want {
		t.Fatalf("got focus index %d, want %d", fm.Index, want)
	}
}

// TestPaneTitleShowsFKeyLabel confirms the "[F#]" hint painted in
// paneNode actually reaches the screen for the first 9 panes.
func TestPaneTitleShowsFKeyLabel(t *testing.T) {
	m := newTestModel(testSpec("a"), testSpec("b"))
	app := tui.NewApp(m, 80, 16)
	defer app.Close()

	buf := app.Buffer().String()
	if !strings.Contains(buf, "[F1]") || !strings.Contains(buf, "[F2]") {
		t.Fatalf("expected both [F1] and [F2] labels on screen:\n%s", buf)
	}
}

func TestSplittingLiveShellPanePreservesItsProcess(t *testing.T) {
	skipUnlessOnPath(t, "sh")
	cmd := []string{"sh", "-c", "echo READY; read x; echo GOT:$x"}
	m := newTestModel(Spec{Title: "test", Argv: cmd})
	id := m.panes[0].id

	app := tui.NewApp(m, 40, 16)
	defer app.Close()

	waitForText(t, app, "READY", 3*time.Second)

	app.Dispatch(splitPaneMsg{id: id, dir: layout.Horizontal, spec: testSpec("kyu")})
	forceRenders(app, 3)

	if strings.Contains(app.Buffer().String(), "failed to start") {
		t.Fatal("split-open pane's process was discarded and failed to restart from an already-consumed exec.Cmd — tui#3's fix should prevent this")
	}
	if !strings.Contains(app.Buffer().String(), "READY") {
		t.Fatalf("original pane's output vanished after splitting it:\n%s", app.Buffer().String())
	}
}

func TestPaneExitedMsgMarksExited(t *testing.T) {
	m := newTestModel(testSpec("a"))
	id := m.panes[0].id
	next, _ := m.Update(paneExitedMsg{id: id})
	m = next.(Model)
	if !m.panes[0].exited {
		t.Fatal("pane should be marked exited")
	}
}

func TestToggleThemeMsgFlipsAppearance(t *testing.T) {
	m := newTestModel(testSpec("a"))
	start := m.theme.Appearance

	next, cmd := m.Update(toggleThemeMsg{})
	if cmd != nil {
		t.Fatal("toggling the theme should not produce a Cmd")
	}
	m = next.(Model)
	if m.theme.Appearance == start {
		t.Fatalf("theme.Appearance unchanged after one toggle (still %v)", start)
	}

	next, _ = m.Update(toggleThemeMsg{})
	m = next.(Model)
	if m.theme.Appearance != start {
		t.Fatalf("theme.Appearance = %v after a second toggle, want back to %v", m.theme.Appearance, start)
	}
}

func TestToggleZoomMsgSetsAndClearsZoomedID(t *testing.T) {
	m := newTestModel(testSpec("a"))
	m, _ = m.splitPane(m.panes[0].id, layout.Horizontal, testSpec("b"))
	idA, idB := m.panes[0].id, m.panes[1].id

	next, cmd := m.Update(toggleZoomMsg{id: idA})
	if cmd != nil {
		t.Fatal("toggling zoom should not produce a Cmd")
	}
	m = next.(Model)
	if m.zoomedID != idA {
		t.Fatalf("zoomedID = %d after zooming A, want %d", m.zoomedID, idA)
	}

	next, _ = m.Update(toggleZoomMsg{id: idB})
	m = next.(Model)
	if m.zoomedID != idB {
		t.Fatalf("zoomedID = %d after zooming B, want %d (should switch, not clear)", m.zoomedID, idB)
	}

	next, _ = m.Update(toggleZoomMsg{id: idB})
	m = next.(Model)
	if m.zoomedID != 0 {
		t.Fatalf("zoomedID = %d after re-toggling B, want 0 (un-zoomed)", m.zoomedID)
	}
}

func TestClosingZoomedPaneClearsZoom(t *testing.T) {
	m := newTestModel(testSpec("a"))
	m, _ = m.splitPane(m.panes[0].id, layout.Horizontal, testSpec("b"))
	idB := m.panes[1].id

	next, _ := m.Update(toggleZoomMsg{id: idB})
	m = next.(Model)
	if m.zoomedID != idB {
		t.Fatal("setup: expected B to be zoomed")
	}

	next, _ = m.Update(closePaneMsg{id: idB})
	m = next.(Model)
	if m.zoomedID != 0 {
		t.Fatalf("zoomedID = %d after closing the zoomed pane, want 0", m.zoomedID)
	}
}

// TestZoomingAPaneKeepsSiblingProcessAlive is zoom's counterpart to
// TestMinimizeKeepsProcessAliveAndStatePreserved.
func TestZoomingAPaneKeepsSiblingProcessAlive(t *testing.T) {
	skipUnlessOnPath(t, "sh")
	cmd := []string{"sh", "-c", "echo READY; read x; echo GOT:$x"}
	m := newTestModel(Spec{Title: "shell", Argv: cmd})
	shellID := m.panes[0].id

	app := tui.NewApp(m, 40, 10)
	defer app.Close()

	waitForText(t, app, "READY", 3*time.Second)

	app.Dispatch(splitPaneMsg{id: shellID, dir: layout.Horizontal, spec: testSpec("kyu")})
	forceRenders(app, 3)
	kyuID := shellID + 1

	app.Dispatch(toggleZoomMsg{id: kyuID})
	forceRenders(app, 3)
	if strings.Contains(app.Buffer().String(), "READY") {
		t.Fatal("the shell pane should be collapsed out of view while a sibling is zoomed")
	}

	app.Dispatch(toggleZoomMsg{id: kyuID})
	waitForText(t, app, "READY", 3*time.Second)

	if strings.Contains(app.Buffer().String(), "failed to start") {
		t.Fatal("the shell pane was disposed and recreated across zoom/un-zoom — its running process was killed")
	}
}

func TestToggleHelpMsgOpensAndCloses(t *testing.T) {
	m := newTestModel(testSpec("a"))
	if m.helpOpen {
		t.Fatal("help should start closed")
	}

	next, _ := m.Update(toggleHelpMsg{})
	m = next.(Model)
	if !m.helpOpen {
		t.Fatal("expected help open after one toggleHelpMsg")
	}

	next, _ = m.Update(toggleHelpMsg{})
	m = next.(Model)
	if m.helpOpen {
		t.Fatal("expected help closed after a second toggleHelpMsg")
	}
}

func TestCloseHelpMsgClosesRegardlessOfState(t *testing.T) {
	m := newTestModel(testSpec("a"))
	next, _ := m.Update(toggleHelpMsg{})
	m = next.(Model)
	if !m.helpOpen {
		t.Fatal("setup: expected help open")
	}
	next, _ = m.Update(closeHelpMsg{})
	m = next.(Model)
	if m.helpOpen {
		t.Fatal("expected help closed after closeHelpMsg")
	}
}

// TestHelpButtonShowsHelpContentOnScreen drives the real input/render
// path (click-equivalent Enter on the help button, via the actual
// control-strip focus order) rather than just Update.
func TestHelpButtonShowsHelpContentOnScreen(t *testing.T) {
	m := newTestModel(testSpec("a"))
	app := tui.NewApp(m, 80, 24)
	defer app.Close()

	// Tab to the "help" button: one per configured preset precedes it.
	for range len(testPresets) {
		app.HandleInput(input.KeyEvent{Key: input.KeyTab})
	}
	for _, cmd := range app.HandleInput(input.KeyEvent{Key: input.KeyEnter}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 1)
	if buf := app.Buffer().String(); !strings.Contains(buf, "9mux — help") {
		t.Fatalf("expected help content on screen after activating the help button:\n%s", buf)
	}
}

func TestQuitRequestedProducesQuitCmd(t *testing.T) {
	m := newTestModel(testSpec("a"))
	_, cmd := m.Update(quitRequestedMsg{})
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd")
	}
	if _, ok := cmd().(tui.QuitMsg); !ok {
		t.Fatalf("Cmd produced %T, want tui.QuitMsg", cmd())
	}
}

func TestClickedRecognizesEnterSpaceAndLeftClick(t *testing.T) {
	cases := []struct {
		name string
		e    input.Event
		want bool
	}{
		{"enter", input.KeyEvent{Key: input.KeyEnter}, true},
		{"space", input.KeyEvent{Rune: ' '}, true},
		{"other key", input.KeyEvent{Rune: 'x'}, false},
		{"left click", input.MouseEvent{Button: input.MouseLeft}, true},
		{"drag", input.MouseEvent{Button: input.MouseLeft, Drag: true}, false},
		{"release", input.MouseEvent{Button: input.MouseRelease}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clicked(c.e); got != c.want {
				t.Errorf("clicked(%v) = %v, want %v", c.e, got, c.want)
			}
		})
	}
}

// ---- integration: real tui.App + widget.Terminal, the part with
// actual keying-correctness risk (see model.go's package doc) ----

func TestMinimizeKeepsProcessAliveAndStatePreserved(t *testing.T) {
	skipUnlessOnPath(t, "sh")
	cmd := []string{"sh", "-c", "echo READY; read x; echo GOT:$x"}
	m := newTestModel(Spec{Title: "test", Argv: cmd})
	id := m.panes[0].id

	app := tui.NewApp(m, 40, 10)
	defer app.Close()

	waitForText(t, app, "READY", 3*time.Second)

	app.Dispatch(toggleMinimizeMsg{id: id})
	forceRenders(app, 3)
	if strings.Contains(app.Buffer().String(), "READY") {
		t.Fatal("minimized pane should not occupy screen space")
	}

	app.Dispatch(toggleMinimizeMsg{id: id})
	waitForText(t, app, "READY", 3*time.Second)

	if strings.Contains(app.Buffer().String(), "failed to start") {
		t.Fatal("pane was disposed and recreated across minimize/restore — the running process was killed")
	}
}

// TestHorizontalSplitPaneCannotMinimize drives the real input path: a
// pane that's a child of a horizontal split shouldn't be minimizable
// at all. Uses F2 to jump straight to the second pane's own title bar,
// then Enter (the normal click-equivalent minimize toggle) should be a
// no-op — both panes' live markers must stay visible.
func TestHorizontalSplitPaneCannotMinimize(t *testing.T) {
	skipUnlessOnPath(t, "sh")
	a := []string{"sh", "-c", "echo MARKERA; read x"}
	b := []string{"sh", "-c", "echo MARKERB; read x"}
	m := newTestModel(Spec{Title: "a", Argv: a})
	id := m.panes[0].id
	m, _ = m.splitPane(id, layout.Horizontal, Spec{Title: "b", Argv: b})

	app := tui.NewApp(m, 80, 16)
	defer app.Close()
	waitForText(t, app, "MARKERA", 3*time.Second)
	waitForText(t, app, "MARKERB", 3*time.Second)

	for _, cmd := range app.HandleInput(input.KeyEvent{Key: input.KeyF2}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	for _, cmd := range app.HandleInput(input.KeyEvent{Key: input.KeyEnter}) {
		if cmd != nil {
			app.Dispatch(cmd())
		}
	}
	forceRenders(app, 2)

	buf := app.Buffer().String()
	if !strings.Contains(buf, "MARKERA") || !strings.Contains(buf, "MARKERB") {
		t.Fatalf("expected both panes' content still visible (minimize should be a no-op here):\n%s", buf)
	}
}

// TestAddingSecondPaneKeepsFirstPaneAlive is the tree-restructuring
// counterpart to TestMinimizeKeepsProcessAliveAndStatePreserved.
func TestAddingSecondPaneKeepsFirstPaneAlive(t *testing.T) {
	skipUnlessOnPath(t, "sh")
	cmd := []string{"sh", "-c", "echo READY; read x; echo GOT:$x"}
	m := newTestModel(Spec{Title: "first", Argv: cmd})

	app := tui.NewApp(m, 40, 10)
	defer app.Close()

	waitForText(t, app, "READY", 3*time.Second)

	app.Dispatch(addPaneMsg{spec: testSpec("second")})
	forceRenders(app, 3)

	if strings.Contains(app.Buffer().String(), "failed to start") {
		t.Fatal("adding a second pane discarded the first pane's retained state — its running process was killed")
	}
	if !strings.Contains(app.Buffer().String(), "READY") {
		t.Fatalf("first pane's output vanished after adding a second pane:\n%s", app.Buffer().String())
	}
}

func TestExitedPaneShowsIndicatorAfterEvent(t *testing.T) {
	skipUnlessOnPath(t, "true")
	cmd := []string{"true"}
	m := newTestModel(Spec{Title: "test", Argv: cmd})

	app := tui.NewApp(m, 40, 10)
	defer app.Close()

	// OnExit fires opportunistically from HandleEvent (see
	// widget.Terminal's doc comment) — repeated no-op renders are
	// enough to eventually observe the exited state once the process
	// has actually exited and a render happens to run after that.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		app.Dispatch(struct{}{})
		if strings.Contains(app.Buffer().String(), "exited") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("exited indicator never appeared:\n%s", app.Buffer().String())
}

// TestExpandSpawnTokensSubstitutesIDAndMuxPID pins the plain-string
// substitution expandSpawnTokens does for a preset's {id} and $MUX_PID
// tokens — no shell involved, so this must work by literal text
// replacement alone.
func TestExpandSpawnTokensSubstitutesIDAndMuxPID(t *testing.T) {
	argv := []string{"9sh", "--listen-unix", "/tmp/9sh-$MUX_PID-{id}.sock"}
	got := expandSpawnTokens(argv, 7)
	want := []string{"9sh", "--listen-unix", "/tmp/9sh-" + strconv.Itoa(muxPID) + "-7.sock"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expandSpawnTokens(%v, 7) = %v, want %v", argv, got, want)
		}
	}
}

// TestNewPaneStateExpandsSpawnTokens confirms a preset-sourced Spec's
// {id} token is resolved against the pane's own id once newPaneState
// actually builds the exec.Cmd — the whole reason Spec carries Argv,
// not a pre-built *exec.Cmd, is so this substitution can happen here,
// after an id exists, rather than back when SpecFromPreset ran.
func TestNewPaneStateExpandsSpawnTokens(t *testing.T) {
	s := Spec{Title: "kyu", Argv: []string{"9sh", "--listen-unix", "/tmp/9sh-{id}.sock"}}
	p := newPaneState(42, s)
	want := "/tmp/9sh-42.sock"
	if got := p.command.Args[2]; got != want {
		t.Fatalf("newPaneState(42, ...).command.Args[2] = %q, want %q", got, want)
	}
}

// TestNewPaneStateExpandsBrowseCompanionTokens confirms a preset-
// sourced Spec's BrowseCompanion address gets the same {id}/$MUX_PID
// substitution as Argv, resolved once at spawn time and stashed on
// paneState.browseCompanion — the address the title bar's 'b' key
// later splits off unchanged (see TestTitleBarBKeyOpensBrowseCompanion).
func TestNewPaneStateExpandsBrowseCompanionTokens(t *testing.T) {
	s := Spec{
		Title:           "kyu",
		Argv:            []string{"9sh", "--listen-unix", "/tmp/9sh-{id}.sock"},
		BrowseCompanion: &BrowseSpec{Network: "unix", Addr: "/tmp/9sh-{id}.sock"},
	}
	p := newPaneState(42, s)
	if p.browseCompanion == nil {
		t.Fatal("expected browseCompanion to be set")
	}
	if want := "/tmp/9sh-42.sock"; p.browseCompanion.Addr != want {
		t.Fatalf("newPaneState(42, ...).browseCompanion.Addr = %q, want %q", p.browseCompanion.Addr, want)
	}
	if p.browseCompanion.Network != "unix" {
		t.Fatalf("newPaneState(42, ...).browseCompanion.Network = %q, want %q", p.browseCompanion.Network, "unix")
	}
}

func forceRenders(app *tui.App, n int) {
	for range n {
		app.Dispatch(struct{}{})
	}
}

func waitForText(t *testing.T, app *tui.App, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		app.Dispatch(struct{}{})
		if strings.Contains(app.Buffer().String(), substr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in buffer:\n%s", substr, app.Buffer().String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
