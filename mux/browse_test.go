package mux

import (
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/9p/examples/memfs"
	"github.com/sandgorgon/9p/server"
	"github.com/sandgorgon/tui/tui"
)

// ---- fixtures: a real 9P server (memfs) reached over a real socket ----

// newBrowseTestServer starts an in-memory 9P server on a loopback TCP
// port, dials a client against it, and attaches — real wire protocol
// end to end, not a mock. Returns the client (for listBrowseCmd et al.
// under test) and the raw root Fid (for building fixture files/dirs,
// which only Fid exposes — see client/fid.go's Create).
func newBrowseTestServer(t *testing.T) (*client.Client, *client.Fid, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &server.Server{FS: memfs.New()}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })

	c, err := client.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	root, err := c.Attach("test", "")
	if err != nil {
		t.Fatal(err)
	}
	return c, root, l.Addr().String()
}

func mkdir(t *testing.T, root *client.Fid, parent []string, name string) {
	t.Helper()
	dir, err := root.Walk(parent...)
	if err != nil {
		t.Fatalf("walk %v: %v", parent, err)
	}
	defer dir.Clunk()
	if _, _, err := dir.Create(name, p9.DMDIR|0755, p9.OREAD); err != nil {
		t.Fatalf("mkdir %v/%s: %v", parent, name, err)
	}
}

func writeFile(t *testing.T, root *client.Fid, parent []string, name string, data []byte) {
	t.Helper()
	dir, err := root.Walk(parent...)
	if err != nil {
		t.Fatalf("walk %v: %v", parent, err)
	}
	defer dir.Clunk()
	f, err := dir.CreateFile(name, 0644, p9.OWRITE)
	if err != nil {
		t.Fatalf("create %v/%s: %v", parent, name, err)
	}
	defer f.Close()
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			t.Fatalf("write %v/%s: %v", parent, name, err)
		}
	}
}

func findEntry(entries []p9.Stat, name string) (p9.Stat, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return p9.Stat{}, false
}

// ---- listBrowseCmd / loadPreviewCmd / killJobCmd against a real server ----

func TestListBrowseCmdPlainDirectory(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	mkdir(t, root, nil, "browsetest")
	mkdir(t, root, []string{"browsetest"}, "sub")
	writeFile(t, root, []string{"browsetest"}, "hello.txt", []byte("hi\nthere"))

	msg := listBrowseCmd(1, c, []string{"browsetest"})()
	lm, ok := msg.(browseListedMsg)
	if !ok {
		t.Fatalf("got %T, want browseListedMsg", msg)
	}
	if lm.err != nil {
		t.Fatalf("unexpected error: %v", lm.err)
	}
	if lm.jobRows != nil {
		t.Fatalf("got job rows %v, want a plain listing", lm.jobRows)
	}
	sub, ok := findEntry(lm.entries, "sub")
	if !ok || !sub.Qid.IsDir() {
		t.Errorf("entries %v: want a dir entry named sub", lm.entries)
	}
	hello, ok := findEntry(lm.entries, "hello.txt")
	if !ok || hello.Qid.IsDir() {
		t.Errorf("entries %v: want a file entry named hello.txt", lm.entries)
	}
}

func TestListBrowseCmdJobShapedDirectory(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	writeFile(t, root, []string{"jobs"}, "clone", nil)
	mkdir(t, root, []string{"jobs"}, "1")
	status := `{"id":1,"kind":"subprocess","state":"running","argv":["echo","hi"],"pid":123}`
	writeFile(t, root, []string{"jobs", "1"}, "status", []byte(status))
	writeFile(t, root, []string{"jobs", "1"}, "ctl", nil)

	msg := listBrowseCmd(1, c, []string{"jobs"})()
	lm, ok := msg.(browseListedMsg)
	if !ok {
		t.Fatalf("got %T, want browseListedMsg", msg)
	}
	if lm.err != nil {
		t.Fatalf("unexpected error: %v", lm.err)
	}
	if lm.entries != nil {
		t.Fatalf("got entries %v, want job rows only", lm.entries)
	}
	if len(lm.jobRows) != 1 || len(lm.jobIDs) != 1 || lm.jobIDs[0] != 1 {
		t.Fatalf("got jobRows=%v jobIDs=%v, want one row for job 1", lm.jobRows, lm.jobIDs)
	}
	if !strings.Contains(lm.jobRows[0], "running") || !strings.Contains(lm.jobRows[0], "echo hi") {
		t.Errorf("job row %q missing expected fields", lm.jobRows[0])
	}
}

func TestListBrowseCmdEmptyJobsDirStillMatchesShape(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	writeFile(t, root, []string{"jobs"}, "clone", nil)

	msg := listBrowseCmd(1, c, []string{"jobs"})()
	lm, ok := msg.(browseListedMsg)
	if !ok {
		t.Fatalf("got %T, want browseListedMsg", msg)
	}
	if lm.jobRows == nil || len(lm.jobRows) != 0 {
		t.Errorf("got jobRows=%v entries=%v, want an empty (non-nil-classified) job table", lm.jobRows, lm.entries)
	}
}

func TestKillJobCmdWritesKillToCtl(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	mkdir(t, root, []string{"jobs"}, "1")
	writeFile(t, root, []string{"jobs", "1"}, "ctl", nil)

	msg := killJobCmd(1, c, []string{"jobs"}, 1)()
	km, ok := msg.(browseKillResultMsg)
	if !ok {
		t.Fatalf("got %T, want browseKillResultMsg", msg)
	}
	if km.err != nil {
		t.Fatalf("unexpected error: %v", km.err)
	}

	f, err := c.Open("/jobs/1/ctl", p9.OREAD)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "kill" {
		t.Errorf("ctl content = %q, want %q", b, "kill")
	}
}

// ---- wait-driven job auto-refresh ----

// step runs one Update call and returns the resulting Model/Cmd
// unresolved — unlike mustUpdate, it never follows the Cmd itself.
// Required for anything touching a wait watch: memfs's Read never
// actually blocks, so a watch on a job that (as these fixtures leave
// it) never reaches a terminal state resolves instantly and re-arms
// itself — mustUpdate's "follow every Cmd to the end" would chase that
// forever. step lets a test control exactly how many rounds of the
// auto-refresh loop to observe.
func step(t *testing.T, m Model, msg tui.Msg) (Model, tui.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	return nm, cmd
}

func makeJob(t *testing.T, root *client.Fid, id int, state string) {
	t.Helper()
	dir := strconv.Itoa(id)
	mkdir(t, root, []string{"jobs"}, dir)
	status := fmt.Sprintf(`{"id":%d,"kind":"subprocess","state":%q}`, id, state)
	writeFile(t, root, []string{"jobs", dir}, "status", []byte(status))
	writeFile(t, root, []string{"jobs", dir}, "wait", nil)
}

func TestJobStatusTerminal(t *testing.T) {
	cases := map[string]bool{
		"pending": false, "running": false, "stopped": false,
		"done": true, "failed": true, "killed": true,
		"": false, "bogus": false,
	}
	for state, want := range cases {
		if got := (jobStatus{State: state}).terminal(); got != want {
			t.Errorf("jobStatus{State:%q}.terminal() = %v, want %v", state, got, want)
		}
	}
}

func TestListBrowseCmdComputesJobTerminalFlags(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	writeFile(t, root, []string{"jobs"}, "clone", nil)
	makeJob(t, root, 1, "running")
	makeJob(t, root, 2, "done")

	msg := listBrowseCmd(1, c, []string{"jobs"})()
	lm, ok := msg.(browseListedMsg)
	if !ok {
		t.Fatalf("got %T, want browseListedMsg", msg)
	}
	if lm.err != nil {
		t.Fatalf("unexpected error: %v", lm.err)
	}
	if len(lm.jobIDs) != 2 || len(lm.jobTerminal) != 2 {
		t.Fatalf("got jobIDs=%v jobTerminal=%v, want 2 entries each", lm.jobIDs, lm.jobTerminal)
	}
	terminal := map[int]bool{}
	for i, id := range lm.jobIDs {
		terminal[id] = lm.jobTerminal[i]
	}
	if terminal[1] {
		t.Errorf("job 1 (running): terminal = true, want false")
	}
	if !terminal[2] {
		t.Errorf("job 2 (done): terminal = false, want true")
	}
}

// connectAndListJobs connects p's browsing pane and lists /jobs,
// stopping right after that listing's own Update call — deliberately
// not resolving whatever watch Cmd it returns (see step's own doc
// comment for why). Returns the model and that unresolved Cmd.
func connectAndListJobs(t *testing.T, m Model, p *paneState) (Model, tui.Cmd) {
	t.Helper()
	id := p.id
	m, rootListCmd := step(t, m, connectCmdForPane(p)())
	if p.browse.client == nil {
		t.Fatalf("connect failed: %s", p.browse.connErr)
	}
	if rootListCmd == nil {
		t.Fatal("expected connecting to trigger a root listing")
	}
	// The root listing itself is never job-shaped in these fixtures
	// (only /jobs is), so this step is safe to run to completion.
	m = mustUpdate(t, m, rootListCmd())
	return step(t, m, listBrowseCmd(id, p.browse.client, []string{"jobs"})())
}

// TestListBrowseCmdLaunchesWatchesForNonTerminalJobsOnly drives a
// job-table listing through Update and confirms only the non-terminal
// job gets a wait watch — the done job doesn't, since it can never
// resolve one again.
func TestListBrowseCmdLaunchesWatchesForNonTerminalJobsOnly(t *testing.T) {
	_, root, addr := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	writeFile(t, root, []string{"jobs"}, "clone", nil)
	makeJob(t, root, 1, "running")
	makeJob(t, root, 2, "done")

	m := newTestModel(Spec{Title: "browse", Browse: &BrowseSpec{Network: "tcp", Addr: addr}})
	p := m.panes[0]
	m, _ = connectAndListJobs(t, m, p) // the watch Cmd itself is left unresolved on purpose
	if !p.browse.watchingJobs[1] {
		t.Errorf("expected job 1 (running) to be watched, watchingJobs=%v", p.browse.watchingJobs)
	}
	if p.browse.watchingJobs[2] {
		t.Errorf("expected job 2 (done) to not be watched, watchingJobs=%v", p.browse.watchingJobs)
	}
}

// TestWaitDrivenAutoRefreshTriggersRelist confirms the whole loop
// composes: a resolved watch clears itself, triggers exactly one
// fresh listing, and that listing (seeing the job still non-terminal)
// re-arms a fresh watch — the auto-refresh keeps working across
// repeated resolutions, not just the first one. It stops there rather
// than resolving that fresh watch too: memfs's Read never actually
// blocks, so with this fixture's job permanently "running," that would
// just be the same cycle forever, not a further-interesting assertion.
func TestWaitDrivenAutoRefreshTriggersRelist(t *testing.T) {
	_, root, addr := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	writeFile(t, root, []string{"jobs"}, "clone", nil)
	makeJob(t, root, 1, "running")

	m := newTestModel(Spec{Title: "browse", Browse: &BrowseSpec{Network: "tcp", Addr: addr}})
	p := m.panes[0]
	id := p.id
	m, _ = connectAndListJobs(t, m, p)
	if !p.browse.watchingJobs[1] {
		t.Fatalf("expected job 1 to be watched after the initial listing, watchingJobs=%v", p.browse.watchingJobs)
	}

	// Resolve the watch. Immediately after — before running whatever
	// follow-up Cmd this produces — the watch is cleared...
	m, relistCmd := step(t, m, browseJobWaitMsg{id: id, path: []string{"jobs"}, jobID: 1})
	if p.browse.watchingJobs[1] {
		t.Fatal("expected job 1's watch to be cleared immediately on resolution")
	}
	if relistCmd == nil {
		t.Fatal("expected a follow-up listBrowseCmd after the watch resolved")
	}

	// ...and running that follow-up re-lists /jobs, sees job 1 still
	// "running", and re-arms a fresh watch for it.
	m, _ = step(t, m, relistCmd())
	if !p.browse.watchingJobs[1] {
		t.Fatal("expected a fresh watch to be re-armed after the auto-refresh")
	}
	if len(p.browse.jobRows) != 1 || !strings.Contains(p.browse.jobRows[0], "running") {
		t.Errorf("expected the refreshed listing to still show job 1 running, got %v", p.browse.jobRows)
	}
}

// TestManualRefreshDoesNotDuplicateWatches confirms pressing 'r' while
// a job's watch is already outstanding doesn't start a second,
// redundant one for the same job.
func TestManualRefreshDoesNotDuplicateWatches(t *testing.T) {
	_, root, addr := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	writeFile(t, root, []string{"jobs"}, "clone", nil)
	makeJob(t, root, 1, "running")

	m := newTestModel(Spec{Title: "browse", Browse: &BrowseSpec{Network: "tcp", Addr: addr}})
	p := m.panes[0]
	id := p.id
	m, _ = connectAndListJobs(t, m, p)
	if !p.browse.watchingJobs[1] {
		t.Fatalf("expected job 1 to be watched")
	}

	m, refreshCmd := step(t, m, browseRefreshMsg{id: id})
	if refreshCmd == nil {
		t.Fatal("expected the refresh to issue a listBrowseCmd")
	}
	_, dupWatchCmd := step(t, m, refreshCmd())
	if dupWatchCmd != nil {
		t.Errorf("expected no new watch Cmd from a refresh that sees an already-watched job, got a non-nil one")
	}
	if len(p.browse.watchingJobs) != 1 || !p.browse.watchingJobs[1] {
		t.Errorf("expected exactly job 1 still watched, got %v", p.browse.watchingJobs)
	}
}

// TestStaleWaitResultIgnoredAfterNavigatingAway confirms a watch that
// resolves after the pane has already moved on to a different
// directory doesn't re-list the wrong (old) path.
func TestStaleWaitResultIgnoredAfterNavigatingAway(t *testing.T) {
	_, root, addr := newBrowseTestServer(t)
	mkdir(t, root, nil, "jobs")
	writeFile(t, root, []string{"jobs"}, "clone", nil)
	makeJob(t, root, 1, "running")
	mkdir(t, root, nil, "other")

	m := newTestModel(Spec{Title: "browse", Browse: &BrowseSpec{Network: "tcp", Addr: addr}})
	p := m.panes[0]
	id := p.id
	m, _ = connectAndListJobs(t, m, p)
	if !p.browse.watchingJobs[1] {
		t.Fatalf("expected job 1 to be watched")
	}

	// Navigate away from /jobs before the watch resolves. "other" is an
	// empty plain directory — never job-shaped — so this listing is
	// safe to run to completion.
	m = mustUpdate(t, m, listBrowseCmd(id, p.browse.client, []string{"other"})())
	if len(p.browse.watchingJobs) != 0 {
		t.Fatalf("expected watchingJobs to reset on navigating to a different directory, got %v", p.browse.watchingJobs)
	}

	// The stale watch for job 1 (under /jobs) now resolves — since the
	// pane has moved on to /other, this must not re-list the wrong
	// directory.
	m, cmd := step(t, m, browseJobWaitMsg{id: id, path: []string{"jobs"}, jobID: 1})
	if cmd != nil {
		t.Error("expected a stale wait result (wrong path) to produce no Cmd")
	}
	if strings.Join(p.browse.path, "/") != "other" {
		t.Errorf("expected path to remain [other], got %v", p.browse.path)
	}
}

// ---- session-history table ----

func TestSessionHistoryFileNames(t *testing.T) {
	dir := func(name string) p9.Stat { return p9.Stat{Name: name, Qid: p9.Qid{Type: p9.QTDIR}} }
	file := func(name string) p9.Stat { return p9.Stat{Name: name} }

	cases := []struct {
		name      string
		stats     []p9.Stat
		wantNames []string
		wantOK    bool
	}{
		{"empty directory", nil, nil, false},
		{
			"unsorted day files come back newest first",
			[]p9.Stat{file("2026-08-28.nrl"), file("2026-09-07.nrl"), file("2026-08-30.nrl")},
			[]string{"2026-09-07.nrl", "2026-08-30.nrl", "2026-08-28.nrl"},
			true,
		},
		{"a subdirectory breaks the shape", []p9.Stat{file("2026-09-07.nrl"), dir("2026-09-08.nrl")}, nil, false},
		{"a non-matching filename breaks the shape", []p9.Stat{file("2026-09-07.nrl"), file("notes.txt")}, nil, false},
		{"almost-right date shape doesn't match", []p9.Stat{file("2026-9-7.nrl")}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			names, ok := sessionHistoryFileNames(c.stats)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && !reflect.DeepEqual(names, c.wantNames) {
				t.Errorf("names = %v, want %v", names, c.wantNames)
			}
		})
	}
}

func TestFormatSessionRow(t *testing.T) {
	exit0 := 0
	cases := []struct {
		name string
		rec  sessionRecord
		want []string // substrings that must all appear in the formatted row
	}{
		{
			"exit code",
			sessionRecord{Host: "Stargazer", Argv: []string{"echo", "hi"}, Exit: &exit0, Kind: "subprocess"},
			[]string{"Stargazer", "exit:0", "echo hi"},
		},
		{
			"signal takes priority over exit",
			sessionRecord{Host: "h", Signal: "KILL", Exit: &exit0, Argv: []string{"sleep", "10"}},
			[]string{"sig:KILL", "sleep 10"},
		},
		{
			"empty argv falls back to kind",
			sessionRecord{Host: "h", Kind: "inproc"},
			[]string{"(inproc)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := formatSessionRow(c.rec)
			for _, want := range c.want {
				if !strings.Contains(row, want) {
					t.Errorf("formatSessionRow(%+v) = %q, want it to contain %q", c.rec, row, want)
				}
			}
		})
	}
}

func writeSessionFile(t *testing.T, root *client.Fid, parent []string, name string, recs []string) {
	t.Helper()
	writeFile(t, root, parent, name, []byte(strings.Join(recs, "\n")+"\n"))
}

func TestListBrowseCmdSessionHistoryDirectory(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	mkdir(t, root, nil, "session")
	mkdir(t, root, []string{"session"}, "history")
	writeSessionFile(t, root, []string{"session", "history"}, "2026-09-06.nrl", []string{
		`{"ts_end":"2026-09-06T10:00:00Z","host":"h","argv":["echo","old"],"exit":0,"kind":"subprocess"}`,
	})
	writeSessionFile(t, root, []string{"session", "history"}, "2026-09-07.nrl", []string{
		`{"ts_end":"2026-09-07T10:00:00Z","host":"h","argv":["echo","first"],"exit":0,"kind":"subprocess"}`,
		`{"ts_end":"2026-09-07T11:00:00Z","host":"h","argv":["echo","second"],"exit":1,"kind":"subprocess"}`,
	})

	msg := listBrowseCmd(1, c, []string{"session", "history"})()
	lm, ok := msg.(browseListedMsg)
	if !ok {
		t.Fatalf("got %T, want browseListedMsg", msg)
	}
	if lm.err != nil {
		t.Fatalf("unexpected error: %v", lm.err)
	}
	if lm.entries != nil || lm.jobRows != nil {
		t.Fatalf("expected only sessionRows populated, got entries=%v jobRows=%v", lm.entries, lm.jobRows)
	}
	if len(lm.sessionRows) != 3 {
		t.Fatalf("got %d session rows, want 3: %v", len(lm.sessionRows), lm.sessionRows)
	}
	// Newest first: 2026-09-07's second record, then its first, then
	// 2026-08-06's — across-file order (newest file first) and
	// within-file order (each file's own records reversed) both matter.
	if !strings.Contains(lm.sessionRows[0], "second") {
		t.Errorf("row 0 = %q, want the newest record (\"second\")", lm.sessionRows[0])
	}
	if !strings.Contains(lm.sessionRows[1], "first") {
		t.Errorf("row 1 = %q, want the next-newest record (\"first\")", lm.sessionRows[1])
	}
	if !strings.Contains(lm.sessionRows[2], "old") {
		t.Errorf("row 2 = %q, want the oldest record (\"old\")", lm.sessionRows[2])
	}
}

// TestLoadSessionRowsStopsBeforeReadingFilesItDoesntNeed confirms the
// early-stop optimization actually skips older files once limit is
// reached, mirroring session.ReadRecent's own algorithm: names[1]
// names a file that was never created on the server, so if
// loadSessionRows tried to open it (i.e. the early stop didn't
// trigger), this would fail with an error instead of succeeding.
func TestLoadSessionRowsStopsBeforeReadingFilesItDoesntNeed(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	mkdir(t, root, nil, "history")
	writeSessionFile(t, root, []string{"history"}, "2026-09-07.nrl", []string{
		`{"ts_end":"2026-09-07T10:00:00Z","host":"h","argv":["a"],"exit":0}`,
		`{"ts_end":"2026-09-07T11:00:00Z","host":"h","argv":["b"],"exit":0}`,
		`{"ts_end":"2026-09-07T12:00:00Z","host":"h","argv":["c"],"exit":0}`,
	})
	// "2026-09-06.nrl" deliberately doesn't exist on the server.
	names := []string{"2026-09-07.nrl", "2026-09-06.nrl"}

	rows, err := loadSessionRows(c, []string{"history"}, names, 2)
	if err != nil {
		t.Fatalf("expected the walk to stop before reaching the nonexistent file, got error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (limit), got %v", len(rows), rows)
	}
	if !strings.Contains(rows[0], "c") || !strings.Contains(rows[1], "b") {
		t.Errorf("got rows %v, want the two newest records (c, then b)", rows)
	}
}

func TestLoadPreviewCmd(t *testing.T) {
	c, root, _ := newBrowseTestServer(t)
	writeFile(t, root, nil, "hello.txt", []byte("line1\nline2\n"))

	msg := loadPreviewCmd(1, c, []string{"hello.txt"})()
	pm, ok := msg.(browsePreviewLoadedMsg)
	if !ok {
		t.Fatalf("got %T, want browsePreviewLoadedMsg", msg)
	}
	if pm.err != nil {
		t.Fatalf("unexpected error: %v", pm.err)
	}
	want := []string{"line1", "line2"}
	if !reflect.DeepEqual(pm.lines, want) {
		t.Errorf("got lines %v, want %v", pm.lines, want)
	}
}

// ---- full pane lifecycle through Model.Update: connect, list, descend
// into a subdirectory, open a file preview, close it, go back up ----

func TestBrowsePaneEndToEnd(t *testing.T) {
	_, root, addr := newBrowseTestServer(t)
	mkdir(t, root, nil, "browsetest")
	mkdir(t, root, []string{"browsetest"}, "sub")
	writeFile(t, root, []string{"browsetest"}, "hello.txt", []byte("hi there"))

	m := newTestModel(Spec{Title: "browse", Browse: &BrowseSpec{Network: "tcp", Addr: addr}})
	p := m.panes[0]
	id := p.id

	// Connect.
	m = mustUpdate(t, m, connectCmdForPane(p)())
	if p.browse.connErr != "" {
		t.Fatalf("connect error: %s", p.browse.connErr)
	}
	if p.browse.client == nil {
		t.Fatal("expected a live client after browseConnectedMsg")
	}

	// Initial listing (root: "browsetest").
	m = mustUpdate(t, m, listBrowseCmd(id, p.browse.client, nil)())
	root0, ok := findEntry(p.browse.entries, "browsetest")
	if !ok || !root0.Qid.IsDir() {
		t.Fatalf("root entries %v: want a dir entry named browsetest", p.browse.entries)
	}

	// Move the cursor onto "browsetest" and descend into it.
	idx := entryIndex(t, p.browse.entries, "browsetest")
	m = mustUpdate(t, m, browseClickMsg{id: id, index: idx})
	m = mustUpdate(t, m, browseEnterMsg{id: id})
	if strings.Join(p.browse.path, "/") != "browsetest" {
		t.Fatalf("path = %v, want [browsetest]", p.browse.path)
	}
	hello, ok := findEntry(p.browse.entries, "hello.txt")
	if !ok || hello.Qid.IsDir() {
		t.Fatalf("browsetest entries %v: want a file entry named hello.txt", p.browse.entries)
	}
	sub, ok := findEntry(p.browse.entries, "sub")
	if !ok || !sub.Qid.IsDir() {
		t.Fatalf("browsetest entries %v: want a dir entry named sub", p.browse.entries)
	}

	// Select "hello.txt" by clicking its rendered row — index 0 is the
	// synthetic ".." row here (len(path) > 0), so the file entries
	// start at 1. This is exactly the offset browseSelectedEntryIndex
	// exists to get right; a regression here would silently open the
	// wrong entry instead of erroring.
	helloRow := 1 + entryIndex(t, p.browse.entries, "hello.txt")
	m = mustUpdate(t, m, browseClickMsg{id: id, index: helloRow})
	m = mustUpdate(t, m, browseEnterMsg{id: id})
	if p.browse.previewPath == "" {
		t.Fatal("expected preview mode after entering a file")
	}
	if got := strings.Join(p.browse.previewLines, "\n"); got != "hi there" {
		t.Errorf("preview lines = %q, want %q", got, "hi there")
	}

	// Close the preview, then go up a directory.
	m = mustUpdate(t, m, browsePreviewCloseMsg{id: id})
	if p.browse.previewPath != "" {
		t.Fatal("expected preview mode to be closed")
	}
	m = mustUpdate(t, m, browseUpMsg{id: id})
	if len(p.browse.path) != 0 {
		t.Fatalf("path = %v, want root after going up", p.browse.path)
	}
	if _, ok := findEntry(p.browse.entries, "browsetest"); !ok {
		t.Fatal("expected root listing again after going up")
	}
}

// mustUpdate feeds msg through m.Update, then recursively resolves and
// feeds back whatever Cmd it produces — including unwrapping a
// tui.BatchMsg's own sub-Cmds one at a time, which Model.Update itself
// never sees (see tui.Batch's own doc comment: "Update never sees a
// BatchMsg" — draining it is normally App.Dispatch's job). This pane
// kind produces one whenever startJobWaitWatches starts more than one
// job's wait watch at once, so a test walking a whole connect->list->
// act chain needs the same unwrapping App.Dispatch would do.
func mustUpdate(t *testing.T, m Model, msg tui.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tui.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				m = mustUpdate(t, m, c())
			}
		}
		return m
	}
	next, cmd := m.Update(msg)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	if cmd != nil {
		return mustUpdate(t, nm, cmd())
	}
	return nm
}

func entryIndex(t *testing.T, entries []p9.Stat, name string) int {
	t.Helper()
	for i, e := range entries {
		if e.Name == name {
			return i
		}
	}
	t.Fatalf("entry %q not found in %v", name, entries)
	return -1
}

// ---- pure unit tests ----

func TestJobDirIDs(t *testing.T) {
	dir := func(name string) p9.Stat { return p9.Stat{Name: name, Qid: p9.Qid{Type: p9.QTDIR}} }
	file := func(name string) p9.Stat { return p9.Stat{Name: name} }

	cases := []struct {
		name    string
		stats   []p9.Stat
		wantIDs []int
		wantOK  bool
	}{
		{"empty jobs dir", []p9.Stat{file("clone")}, nil, true},
		{"two jobs, unsorted input", []p9.Stat{file("clone"), dir("2"), dir("1")}, []int{1, 2}, true},
		{"missing clone", []p9.Stat{dir("1")}, nil, false},
		{"non-numeric dir", []p9.Stat{file("clone"), dir("foo")}, nil, false},
		{"extra file alongside clone", []p9.Stat{file("clone"), dir("1"), file("readme")}, nil, false},
		{"plain directory", []p9.Stat{dir("sub"), file("hello.txt")}, nil, false},
		{"clone that's a directory doesn't count", []p9.Stat{dir("clone")}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids, ok := jobDirIDs(c.stats)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && !reflect.DeepEqual(ids, c.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, c.wantIDs)
			}
		})
	}
}

func TestJoinPath(t *testing.T) {
	if got := joinPath(nil); got != "/" {
		t.Errorf("joinPath(nil) = %q, want /", got)
	}
	if got := joinPath([]string{"a", "b"}); got != "/a/b" {
		t.Errorf("joinPath([a b]) = %q, want /a/b", got)
	}
}

func TestBrowseRowCount(t *testing.T) {
	root := &browseState{entries: make([]p9.Stat, 2)}
	if got := browseRowCount(root); got != 2 {
		t.Errorf("root dir: got %d, want 2 (no .. row at root)", got)
	}
	nested := &browseState{entries: make([]p9.Stat, 2), path: []string{"sub"}}
	if got := browseRowCount(nested); got != 3 {
		t.Errorf("nested dir: got %d, want 3 (.. row plus 2 entries)", got)
	}
	jobs := &browseState{jobRows: []string{"a", "b", "c"}}
	if got := browseRowCount(jobs); got != 3 {
		t.Errorf("job table: got %d, want 3", got)
	}
}

func TestBrowseSelectedEntryIndex(t *testing.T) {
	root := &browseState{entries: make([]p9.Stat, 2)}
	if idx, ok := browseSelectedEntryIndex(root); !ok || idx != 0 {
		t.Errorf("root, cursor 0: got (%d,%v), want (0,true) — no .. offset at root", idx, ok)
	}

	nested := &browseState{entries: make([]p9.Stat, 2), path: []string{"sub"}, cursor: 0}
	if _, ok := browseSelectedEntryIndex(nested); ok {
		t.Error("nested dir, cursor on the .. row: got ok=true, want false")
	}
	nested.cursor = 1
	if idx, ok := browseSelectedEntryIndex(nested); !ok || idx != 0 {
		t.Errorf("nested dir, cursor 1: got (%d,%v), want (0,true) — first real entry", idx, ok)
	}
}
