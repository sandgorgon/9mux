# Design notes, history, and lineage

This is contributor/historical context — why 9mux is shaped the way
it is, and where its code came from. If you just want to use 9mux, see
[../README.md](../README.md); for config syntax see
[CONFIGURATION.md](CONFIGURATION.md).

## Design notes

- **Almost every pane is the same kind: a command in a pty.** No `Kind`
  enum, no general native (non-pty) widget mechanism — unlike the 9sh
  pane package this was extracted from, which had 5 fixed kinds
  (shell, kyu-repl, namespace-browser, job-viewer, session-viewer),
  only one of which (`KindShell`) was actually pty-hosted. 9mux keeps
  that one, made generic, plus exactly one more: the 9P-browsing pane
  (`mux/browse.go`), a deliberate, narrowly-scoped exception — see "The
  9P-browsing pane's design record" and "Lineage" below for why this
  one case earns a second pane kind where nothing else does.
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
  "The 9P-browsing pane's design record" below for exactly where that
  line is and why it's drawn there.

## The 9P-browsing pane's design record

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
same JSON shape 9sh's `job.Status` writes — see [NUANCES.md](NUANCES.md));
and, once 9sh started binding `/session` over plain `dirfs` (9sh's own
commit `cc1302a`), a session-history table aggregated across its
day-sharded log files, the same structural-detection treatment.
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
