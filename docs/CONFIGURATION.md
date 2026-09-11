# Configuration

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

Each preset gets a `+ <name>` control-strip button. The title bar's
`d`/`r` split flow picks a preset by **digit** (1-9, in config order),
not a per-preset letter — see [DESIGN.md](DESIGN.md) for why.

## Per-pane 9P addresses: `{id}` and `$MUX_PID`

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

## Companion browse presets

That still means hand-plugging the actual pid/id into a second config
line (or dialing manually) — a `browse` preset's address is a plain
static string, not itself run through `{id}`/`$MUX_PID` expansion, so
9mux never correlates it with the pane that spawned a particular
socket. A command preset can instead declare a *companion* 9P address,
using the same template, via a second `<name>.browse = ...` line right
after it:

```
kyu = 9sh --listen-unix /tmp/9sh-$MUX_PID-{id}.sock
kyu.browse = unix:/tmp/9sh-$MUX_PID-{id}.sock
```

Both lines are resolved together, once, at that pane's own spawn time
(same `{id}`, same `$MUX_PID`), so the companion address always
matches the socket that pane's own process actually bound. That pane's
title bar then gets one more action, `b` — alongside `x`/`d`/`r`/`z`/
`+`/`-` — which starts the same two-step flow as `d`/`r` itself: `b`
picks that this split will be a browsing pane, then `d` or `r` (or
anything else to cancel) picks its direction, splitting off a browsing
pane pointed at exactly that address. No separate `browse` preset to
hand-write or keep in sync. `b` only shows up in a pane's title-bar
hint when its preset declared a companion; it's a no-op everywhere
else.

See [../README.md](../README.md) for how to use a browsing pane once
you have one open, and [NUANCES.md](NUANCES.md) for the structural
rules that decide what a browsing pane renders (plain listing vs. job
table vs. session-history table) and their caveats.
