# 9mux

[![CI](https://github.com/sandgorgon/9mux/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/sandgorgon/9mux/actions/workflows/ci.yml?query=branch%3Amaster)
[![Release](https://img.shields.io/github/v/release/sandgorgon/9mux)](https://github.com/sandgorgon/9mux/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/sandgorgon/9mux.svg)](https://pkg.go.dev/github.com/sandgorgon/9mux)

9mux is a terminal multiplexer: it runs multiple processes side by
side in a resizable, splittable grid of panes inside one terminal
window, the same shape of tool as `tmux` or `screen`. What's different
is what a pane can be. Almost every pane hosts a real pty-attached
process — a shell, [9sh](https://github.com/sandgorgon/9sh), `htop`,
`vim` directly, anything exec'able — chosen from a small set of
named presets you configure once. Alongside that, one native pane
kind *browses a 9P filesystem* instead of running a process at all —
a live directory listing, job table, or session-history view served
over the [9P protocol](https://en.wikipedia.org/wiki/9P_(protocol))
(see "The 9P-browsing pane" below). 9mux itself has no notion of what
it's hosting: no dependency on kyu, a namespace, or any particular
shell.

Generic pane-hosting is the baseline capability, not the mission. The
actual reason this project exists is to be a better multiplexer
specifically for 9sh/9ed/9vcs/kyu and whatever other namespace-native
tools follow — the 9P-browsing pane below is the concrete piece that
makes that true. See [docs/DESIGN.md](docs/DESIGN.md) for the full
reasoning and lineage.

## Status

Working: the generic split-tree multiplexer — split/resize/minimize/
zoom/close, F1-F9 focus-jump, a configurable control strip, real pty
hosting via `widget.Terminal` — and the 9P-browsing pane: a directory
listing, job table, or session-history table (see below) reached over
`github.com/sandgorgon/9p`, pointed at a running 9sh's `-listen-unix`
socket (or any other 9P server), with wait-driven auto-refresh for
non-terminal jobs (no polling — see [docs/NUANCES.md](docs/NUANCES.md)).
`go build`/`go vet`/`go test ./...` all clean; `mux/model_test.go` is a
full port of 9sh's own `pane/model_test.go` test suite (see
[docs/DESIGN.md](docs/DESIGN.md)), adapted for the Kind-free/
preset-based design, and `mux/browse_test.go` exercises the browsing
pane — listing, preview, job tables, kill, the wait-driven refresh
loop, and session-history aggregation — against a real in-memory 9P
server (`p9/examples/memfs` + `p9/server`), not a mock.

Not built yet: full live-tailing of a job's actual output
(`stdout`/`stderr`/`events` — see [docs/NUANCES.md](docs/NUANCES.md)
for how this differs from the auto-refresh that *is* built).

## Quick start

```
go build -o 9mux ./cmd/9mux
./9mux
```

That's it — a fresh install has no config file yet, so you get one
pane running `$SHELL` and nothing else to set up (see
`config.EnsureDefault`). From there:

- Press **`?`** any time to open 9mux's built-in help screen (the
  keybindings below, always up to date, always in your terminal).
- Press **`d`** or **`r`** on a pane's title bar to split it, then a
  digit to pick what runs in the new pane.
- Press **`x`** on a pane's title bar to close it.

To run something other than your shell — 9sh, `htop`, a browsing pane
pointed at a 9P server — add presets to `~/.config/9mux/config`; see
[Configuration](#configuration) below.

## Keybindings

### Control strip (always visible, top row)

| Key / control | Action |
| --- | --- |
| `+ <preset>` | Add a pane running that configured preset |
| `theme` | Toggle light/dark |
| `quit` | Quit 9mux |

### Every pane's title bar

| Key | Action |
| --- | --- |
| `x` | Close this pane |
| `d` | Split down, then pick a preset by digit (anything else cancels) |
| `r` | Split right (same digit-picker as `d`) |
| `z` | Zoom/un-zoom this pane to fill the whole screen |
| `b` | *(command presets with a paired `<name>.browse =` config line only)* then `d`/`r` for direction: split off a 9P-browsing pane pointed at this pane's own socket |
| `+` / `-` | Resize this pane along its split axis, down to one visible content line — smaller than that, minimize instead |
| click / Enter | Minimize/restore (only along a vertical split axis) |
| `F1`-`F9` | Jump keyboard focus straight to pane N |

### Inside a Terminal pane, once its content has focus

`Tab` reaches the hosted process directly (real completion, if it has
any); `Ctrl+\` releases focus back to pane navigation.

### Inside a 9P-browsing pane

| Key | Action |
| --- | --- |
| `Up`/`Down` / click | Move the cursor |
| `Enter` | Descend into a directory, or preview a file |
| `Backspace` | Go up a directory (or close a file preview) |
| `r` | Refresh the current listing |
| `k` | *(job table only)* Send a kill command to the selected job |
| `Esc` / `Backspace` | Close a file preview |
| `PgUp`/`PgDown` / wheel | Scroll a file preview |

A job table also auto-refreshes on its own whenever a non-terminal job
finishes — no need to press `r` just to notice that. A directory of
day-sharded history logs (9sh's `/session`) renders as an aggregated,
newest-first history table instead of a plain listing. See "The
9P-browsing pane" below and [docs/NUANCES.md](docs/NUANCES.md) for how
that detection works and its limitations.

## Configuration

Presets live in `~/.config/9mux/config`, one `name = value` per line —
either a command preset (`name = argv...`) or a browsing preset
(`name = browse unix:<path>` or `name = browse tcp:<host:port>`):

```
shell = $SHELL
kyu = 9sh
top = htop
jobs = browse unix:/run/user/1000/9sh.sock
```

`$SHELL` expands to the real environment variable (falling back to
`/bin/sh`). Each preset gets a `+ <name>` control-strip button, and the
title bar's `d`/`r` split flow picks a preset by digit (1-9, in config
order).

That covers the common case. 9mux also supports per-pane 9P addresses
via `{id}`/`$MUX_PID` template expansion and "companion" browse presets
that follow a command pane automatically — see
[docs/CONFIGURATION.md](docs/CONFIGURATION.md) for the full syntax and
worked examples.

## The 9P-browsing pane

### Setting one up

A browsing pane is just another preset — it connects to a 9P server
that's already listening somewhere, it doesn't start one. Simplest
case, pointed at a 9sh you start yourself:

```
# terminal 1: start the thing that speaks 9P
9sh --listen-unix /tmp/9sh.sock

# ~/.config/9mux/config
jobs = browse unix:/tmp/9sh.sock
```

Run `./9mux`, click (or Tab/Enter to) the `+ jobs` button on the
control strip, and that pane opens connected to `/tmp/9sh.sock` —
showing 9sh's job table if it has one, a plain listing otherwise. A
`browse tcp:<host:port>` address works the same way for a 9P server
reachable over TCP instead of a Unix socket.

The catch with a fixed path like `/tmp/9sh.sock`: it only matches
*one* running 9sh, and you have to hand-write that path into both the
9sh invocation and the `browse` preset yourself. If you're spawning
the 9sh (or other 9P server) pane *from 9mux itself* rather than
starting it externally, use a **companion browse preset** instead —
9mux generates a fresh, unique socket path per pane and wires the
browsing side to it automatically, no path to keep in sync by hand:

```
kyu = 9sh --listen-unix /tmp/9sh-$MUX_PID-{id}.sock
kyu.browse = unix:/tmp/9sh-$MUX_PID-{id}.sock
```

Open a `kyu` pane, then press `b` on its title bar (only shown because
this preset has a companion), then `d` or `r` for a direction: a
browsing pane splits off, pointed at exactly that `kyu` pane's own
socket. See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for how the
`{id}`/`$MUX_PID` template expansion works and more examples.

### What you'll see

A directory is rendered as a **job table** instead of a plain listing
whenever its shape structurally matches 9sh's job-control protocol —
so this works against any 9P server shaped that way, not just 9sh. In
a job table, `k` writes a kill command to the selected job's `ctl`
file — the one write affordance this pane offers.

A directory of day-sharded history-log files (matching 9sh's
`session` layout) instead renders as an aggregated, newest-first
history table (timestamp, host, exit/signal, argv), capped at 200
records and refreshed on demand like everything except the job table.

Real I/O only ever happens on explicit actions (Enter/Backspace/`r`)
plus one deliberate exception — a job table's auto-refresh when a
watched job finishes — never a background poll. See
[docs/NUANCES.md](docs/NUANCES.md) for exactly how the job/session
table detection works, why the pane doesn't poll, and a known
limitation around canceling an in-flight job watch.

## Design notes, history, and lineage

Why 9mux has only one native pane kind beyond a plain pty, why presets
are digit-picked instead of letter-mnemonic'd, the 9P-browsing pane's
full design record (including approaches that were considered and
rejected), and where this codebase's split-tree mechanics were
extracted from — all in [docs/DESIGN.md](docs/DESIGN.md).

## Dependencies

`github.com/sandgorgon/tui` (`cell`, `input`, `layout`, `style`, `tui`,
`widget` — no `pty` import needed directly here; `widget.Terminal`
wraps it) and `github.com/sandgorgon/9p` (`p9`, `p9/client` — the
9P-browsing pane's own wire protocol and connection layer). Nothing
else — deliberately never a dependency on 9sh itself.
