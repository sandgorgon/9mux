# 9mux

[![CI](https://github.com/sandgorgon/9mux/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/sandgorgon/9mux/actions/workflows/ci.yml?query=branch%3Amaster)
[![Release](https://img.shields.io/github/v/release/sandgorgon/9mux)](https://github.com/sandgorgon/9mux/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/sandgorgon/9mux.svg)](https://pkg.go.dev/github.com/sandgorgon/9mux)

A Plan-9-flavored terminal multiplexer. Almost every pane hosts a real
pty-attached process — a shell, [9sh](https://github.com/sandgorgon/9sh),
`htop`, `vim` directly, anything exec'able — chosen from a small set of
named presets, plus one native pane kind that browses a 9P root instead
(see "The 9P-browsing pane" below). 9mux itself has no notion of what
it's hosting: no dependency on kyu, a namespace, or any particular
shell.

Generic pane-hosting is the baseline capability, not the mission. The
actual reason this project exists is to be a better multiplexer
specifically for 9sh/9ed/9vcs/kyu and whatever other namespace-native
tools follow — the 9P-browsing pane below is the concrete piece that
makes that true.

## Status

Working: the generic split-tree multiplexer — split/resize/minimize/
zoom/close, F1-F9 focus-jump, a configurable control strip, real pty
hosting via `widget.Terminal` — and the 9P-browsing pane: a directory
listing, job table, or session-history table (see below) reached over
`github.com/sandgorgon/9p`, pointed at a running 9sh's `-listen-unix`
socket (or any other 9P server), with wait-driven auto-refresh for
non-terminal jobs (no polling — see below). `go build`/`go vet`/
`go test ./...` all clean; `mux/model_test.go` is a full port of 9sh's
own `pane/model_test.go` test suite (see "Lineage" below), adapted for
the Kind-free/preset-based design, and `mux/browse_test.go` exercises
the browsing pane — listing, preview, job tables, kill, the wait-driven
refresh loop, and session-history aggregation — against a real
in-memory 9P server (`p9/examples/memfs` + `p9/server`), not a mock.

Not built yet: full live-tailing of a job's actual output
(`stdout`/`stderr`/`events` — see "The 9P-browsing pane" for how this
differs from the auto-refresh that *is* built).

## Using it

```
go build -o 9mux ./cmd/9mux
./9mux
```

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
`/bin/sh`) — the one piece of built-in expansion, since it's the only
value that can't be known until run time. A fresh install gets just
`shell = $SHELL` (see `config.EnsureDefault`), so 9mux is immediately
useful with zero configuration even without 9sh installed at all.

Two more tokens expand in a preset's argv, both resolved fresh for
each pane rather than once at config-load time like `$SHELL` above:
`{id}` is that pane's own id (unique within this 9mux process) and
`$MUX_PID` is this 9mux process's own pid (unique across multiple
concurrently running 9mux instances). Together they're enough to give
every pane its own 9P namespace — e.g. a preset like

```
kyu = 9sh --listen-unix /tmp/9sh-$MUX_PID-{id}.sock
```

spawns each `kyu` pane pointed at its own socket, so a `browse
unix:/tmp/9sh-<pid>-<id>.sock` preset (or a manually-run `9p` client)
can address one specific pane's namespace instead of whichever `9sh`
happened to bind a shared default socket last. Plain string
substitution, not shell expansion — argv is exec'd directly, no shell
in between — so it works the same whether or not `sh` is even
installed.

Each preset gets a `+ <name>` control-strip button. The title bar's
`d`/`r` split flow picks a preset by **digit** (1-9, in config order),
not a per-preset letter — see "Design notes" for why.

### The 9P-browsing pane

Inside a browsing pane: Up/Down or a click moves the cursor, Enter
descends into a directory or opens a file preview, Backspace goes up a
directory (or closes a preview), and `r` refreshes — real I/O only ever
happens on these explicit actions plus one deliberate exception (job
auto-refresh, below), never a background poll — matching 9sh's own
now-removed job/namespace viewer panes (see "Lineage") for everything
else, which made the same "a quick glance, not a live dashboard" choice
on purpose.

A directory is rendered as a **job table** instead of a plain listing
whenever its shape structurally matches 9sh's job-control protocol
(`job/fs.go`: a `clone` file plus one numerically-named directory per
job) — not by matching a hardcoded `/jobs` path, so this works against
any 9P server shaped that way, not just 9sh. In a job table, `k` writes
a kill command to the selected job's `ctl` file — the one write
affordance this pane offers; anything more (stop/resume, editing
`argv`/`env`/`cwd` in place) is still open.

**Job auto-refresh.** Every non-terminal job (state other than
`done`/`failed`/`killed`) visible in a job table gets a blocking read on
its own `wait` file (`job/fs.go`'s `waitFile` — resolves the instant
that job reaches a terminal state, per `job.WaitFor`), which on return
triggers exactly one fresh listing of that directory — so a job table
updates itself the moment a watched job finishes, no manual `r` needed
for that one, most common event ("is my job done yet"). This is a
deliberate, narrow exception to the "no background poll" rule above:
one blocking wait per non-terminal job, deduplicated (re-listing the
same directory — a manual `r`, or another job's own auto-refresh —
never starts a second watch for a job already being watched), reset
when the pane navigates to a different directory. Full output
tailing (`stdout`/`stderr`/`events`) is a different, bigger feature
than this and isn't built (see "Status").

Known limitation: `p9/client`'s `File.Read` takes no caller-supplied
context, so an in-flight wait watch can't be canceled individually —
navigating away from a directory before a watched job finishes leaves
that watch (one open fid, one blocked goroutine) running until either
the job actually finishes or the pane's connection closes entirely
(which does unblock every outstanding watch at once, via the same
`io.Closer` mechanism `widget.Terminal` uses for its pty). Bounded —
each watch resolves on its own eventually — but real; worth knowing
before watching many jobs across many directory visits in one
long-lived pane.

**Session-history table.** A directory whose entries are all regular
files matching 9sh's day-sharded history-log naming (`session/
session.go`'s `dayShard`: `YYYY-MM-DD.nrl`) renders as an aggregated,
newest-first history table (timestamp, host, exit/signal, argv) instead
of a raw file listing — the same structural, not path-name-based,
detection the job table uses. This is what lets the browsing pane reach
9sh's `/session` (bound via plain `dirfs` over the real on-disk session
repo, since 9sh's own commit `cc1302a`) usefully, rather than just
showing raw JSON-lines file content in the file preview. Capped at 200
records and, since an append-only log has no single "resolves once"
file the way a job's `wait` does, plain refresh-on-demand (`r`) like
everything but the job table — no auto-refresh, no drill-down (same as
the job table), no write affordance (this is a read-only history, not a
live resource).

## Design notes

- **Almost every pane is the same kind: a command in a pty.** No `Kind`
  enum, no general native (non-pty) widget mechanism — unlike the 9sh
  pane package this was extracted from, which had 5 fixed kinds
  (shell, kyu-repl, namespace-browser, job-viewer, session-viewer),
  only one of which (`KindShell`) was actually pty-hosted. 9mux keeps
  that one, made generic, plus exactly one more: the 9P-browsing pane
  (`mux/browse.go`), a deliberate, narrowly-scoped exception — see "The
  9P-browsing pane" above and "Lineage" below for why this one case
  earns a second pane kind where nothing else does.
- **Presets are digit-picked, not letter-mnemonic'd, in the split
  flow.** The original design's `s`/`k`/`b`/`j`/`h` letters worked
  because there were exactly 5 fixed kinds. Presets here are an
  arbitrary, user-configured, unbounded list — digits (1-9, matching
  the same F1-F9 pane-jump convention already used elsewhere) are the
  only mapping that doesn't need per-preset mnemonic assignment or
  collision handling.
- **The redraw tick isn't Kind-gated.** The original needed to check
  "is this pane a live shell" before starting its periodic redraw tick
  (`widget.Terminal`'s pty output updates its internal state in a
  background goroutine, invisible until the App renders a frame for
  any other reason). Here, every pane is that kind, so the tick simply
  runs whenever any pane exists at all — see `mux/model.go`'s
  `redrawTickCmd`/`withTickIfNeeded`.
- **No dependency on 9sh, or anything 9sh-specific,** by design — see
  "Where this is headed" for exactly where that line is and why it's
  drawn there.

## Where this came from: the 9P-browsing pane's design record

9sh's namespace browser, job viewer, and session viewer panes (the
`pane` package's other four kinds, not carried into this project) were
9sh-specific *by data source* — they read `*eval.Env`/`*ns.Namespace`
directly, in-process — but not by *what they show*: a live directory
listing, a live job table, a history log. 9sh already speaks 9P and
already serves its own namespace over a local Unix socket
(`-listen-unix`, package `remote`'s `ListenUnix`).

What's built: one native pane kind (`mux/browse.go`) — "browse a 9P
root" — on the already-independent `github.com/sandgorgon/9p` client
library, with three renderings: a plain directory listing; a table view
when a directory's shape structurally matches `/jobs` (keyed on the
same JSON shape 9sh's `job.Status` writes — see "The 9P-browsing pane"
above); and, once 9sh started binding `/session` over plain `dirfs`
(9sh's own commit `cc1302a`), a session-history table aggregated across
its day-sharded log files, the same structural-detection treatment.
Pointed at a running 9sh's socket, this reproduces all three of the
namespace-browser/job-viewer/session-viewer panes' capability while
9mux itself only ever depends on `9p` (a sibling library, independent
of `kyu`/`eval`/`ns`), never on 9sh directly — at the real cost of that
decoupling: every listing/read is now an actual 9P wire round-trip
(`client.Fid.Walk`/`Open`/`Read`) rather than 9sh's old in-process
`server.File` calls, so this pane deliberately stays refresh-on-demand
(matching the original `jobviewer.go`'s own choice not to poll) rather
than a live dashboard — session history included, since it has no
per-record "resolves once" file the way a job's `wait` does to hang a
narrow auto-refresh exception off of.

This is genuinely a *better* answer than "9mux depends on 9sh as a Go
library" would be: any 9P server becomes browsable this way, not just
9sh, and the two projects stay decoupled at the module level. It only
covers *read* views plus one narrow write: `k` in a job table writes a
kill command to the selected job's `ctl` file — the minimum useful
case, not the general "write to this 9P file" affordance a fuller
job-control UI (stop/resume, editing `argv`/`env`/`cwd` in place) would
still need.

**Three other approaches were considered and explicitly rejected** —
worth reading before re-proposing them:
1. *9sh ships its own pty-hosted mini-views* (`9sh -view jobs`, hosted
   as an ordinary Terminal pane, identical to running `htop`). Simplest,
   zero new protocol — but every such view is a separate process with
   its own re-implemented mini-TUI, and no live "instant update" feel
   beyond whatever polling it does itself. Kept as the fallback for
   anything that doesn't fit the "browse a namespace" shape.
2. *A generic pane-provider plugin protocol* (a JSON descriptor any
   tool can register: name, launch command, icon/label). More reusable
   in the abstract, but it's a real interface to design and keep
   stable for a benefit nothing concrete needs yet.
3. *9mux imports 9sh's `eval`/`ns` packages as a Go library* and
   re-implements native pane kinds against them directly. Keeps the
   instant, shared-memory feel those views have today inside 9sh, but
   re-couples the two projects at the module level — 9mux could no
   longer be built or used without 9sh's source, undoing the actual
   point of splitting them apart.

## Lineage

This project's generic mechanics (`mux/model.go`'s split-tree layout/
reconciliation/resize/minimize/zoom/focus-nav, `mux/flatfocus.go`) were
extracted from `github.com/sandgorgon/9sh`'s `pane` package at commit
`4b488a7` ("Prepare v0.4.22: bump tui to v0.6.0, use Frameless on list
panes") — the last commit before 9sh started building pane-multiplexer-
specific, in-process fullscreen-program handling that doesn't belong
here. If you need to compare against the original for any reason:

```
git -C <path-to-9sh-checkout> show 4b488a7:pane/model.go
git -C <path-to-9sh-checkout> show 4b488a7:pane/flatfocus.go
git -C <path-to-9sh-checkout> show 4b488a7:pane/help.go
git -C <path-to-9sh-checkout> show 4b488a7:cmd/9sh/main.go
```

`mux/browse.go`'s interaction design (list/preview mode split, cursor
handling, Backspace-to-go-up, mouse click support) is likewise adapted
from that same commit's `pane/browser.go`, `pane/jobviewer.go`, and
`pane/sessionviewer.go` — the namespace-browser, job-viewer, and
session-viewer kinds' own UI (the session-history table's row format in
particular is close to a direct port of `sessionviewer.go`'s own
`formatSessionRow`), ported from direct `server.File`/local-disk calls
to `p9/client` wire calls rather than reinvented from scratch:

```
git -C <path-to-9sh-checkout> show 4b488a7:pane/browser.go
git -C <path-to-9sh-checkout> show 4b488a7:pane/jobviewer.go
git -C <path-to-9sh-checkout> show 4b488a7:pane/sessionviewer.go
```

9sh itself no longer has a multi-pane multiplexer as of the commit
that follows this split (its own `pane` package was removed, replaced
by a single-screen interactive TUI plus its existing `-repl` mode,
each pane meant to run inside a project like this one instead). See
that repo's own README/CHANGELOG for its current shape.

## Dependencies

`github.com/sandgorgon/tui` (`cell`, `input`, `layout`, `style`, `tui`,
`widget` — no `pty` import needed directly here; `widget.Terminal`
wraps it) and `github.com/sandgorgon/9p` (`p9`, `p9/client` — the
9P-browsing pane's own wire protocol and connection layer). Nothing
else — deliberately never a dependency on 9sh itself.
