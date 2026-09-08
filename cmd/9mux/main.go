// Command 9mux is a Plan-9-flavored terminal multiplexer: any number
// of panes, each hosting a real pty-attached process (a shell, 9sh,
// htop, vim directly, ...) chosen from a small set of named presets
// (see package config). It has no notion of what it's hosting — no
// dependency on kyu, a namespace, or any particular shell — that's the
// whole point: it's the generic multiplexer 9sh (and 9ed, 9vcs, and
// whatever else follows) run inside, not a part of any one of them.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sandgorgon/tui/input"
	"github.com/sandgorgon/tui/tui"

	"github.com/sandgorgon/9mux/config"
	"github.com/sandgorgon/9mux/mux"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	showVersion := flag.Bool("version", false, "print the 9mux version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("9mux " + version)
		return 0
	}

	if err := config.EnsureDefault(); err != nil {
		fmt.Fprintln(os.Stderr, "9mux: writing default config:", err)
		// Not fatal -- config.Load below falls back to a built-in
		// default preset list if the file genuinely isn't there.
	}
	presets, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "9mux: loading config:", err)
		return 1
	}
	if len(presets) == 0 {
		fmt.Fprintln(os.Stderr, "9mux: no presets configured and no default available")
		return 1
	}

	m := mux.New(presets, os.Getenv, mux.SpecFromPreset(presets[0]))
	app := tui.NewApp(m, 80, 24) // Run resizes to the real terminal size on start
	defer app.Close()

	// Land keyboard focus on the first pane's own content before Run
	// ever reads real input, not tui.App's zero-value default (the
	// control strip's first button) — see mux.Model.InitialFocusAdvances'
	// doc comment for why this needs replaying real Tab events rather
	// than a simpler fix.
	for i := 0; i < m.InitialFocusAdvances(); i++ {
		app.HandleInput(input.KeyEvent{Key: input.KeyTab})
	}

	fmt.Print(enableMouse)
	defer fmt.Print(disableMouse)
	fmt.Print(enablePaste)
	defer fmt.Print(disablePaste)

	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "9mux:", err)
		return 1
	}
	return 0
}

// SGR mouse reporting isn't on by default -- tui.App.Run doesn't enable
// it itself (a hosted Terminal pane's own mouse-aware program, e.g.
// vim, wants raw mouse bytes forwarded to it just like a real terminal
// would, not tui swallowing them for its own click handling only), so
// this process's own click-to-focus/scroll handling needs it turned on
// around the whole session by hand instead.
const (
	enableMouse  = "\x1b[?1000h\x1b[?1006h"
	disableMouse = "\x1b[?1006l\x1b[?1000l"
	enablePaste  = "\x1b[?2004h"
	disablePaste = "\x1b[?2004l"
)
