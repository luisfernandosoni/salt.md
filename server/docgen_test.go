package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// #79: an editor's copy of a page must never land on top of a rewrite made
// from outside the editor (an agent's write_content, a restore, an import).
// Everything here goes through the real router and the real socket, the way a
// browser tab and an agent reach them, and only through what a client can
// see: these tests were written to fail on 1.1.1 for the reason the issue
// describes, not because a helper is missing.

type editorTab struct {
	t      *testing.T
	srv    *httptest.Server
	cookie string
	page   string
}

func newEditorTab(t *testing.T) (*Server, *editorTab, *user) {
	t.Helper()
	s := testServer(t)
	uid, cookie := signedIn(t, s, "tab@example.test")
	page := s.makePage(t, s.firstWorkspaceOf(t, uid), uid, "", "Probe", `{}`)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, &editorTab{t: t, srv: srv, cookie: cookie, page: page}, &user{ID: uid, Name: "Test"}
}

// open connects an editor the way collab.ts does and reads up to the end of
// the initial replay. err is the close the server answered with, if any.
func (e *editorTab) open(query string) (*websocket.Conn, map[string]any, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/collab/"+e.page+query,
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {e.cookie}}})
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	e.t.Cleanup(func() { c.CloseNow() })
	var hello map[string]any
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return c, hello, err
		}
		if typ != websocket.MessageText {
			continue
		}
		var m map[string]any
		json.Unmarshal(data, &m)
		if _, ok := m["isNew"]; ok {
			hello = m
		}
		if m["synced"] == true {
			return c, hello, nil
		}
	}
}

// typeInto sends a keystroke's worth of Yjs update. The relay never parses CRDT
// data, so any bytes do; what matters is that it is stored as the live doc.
func (e *editorTab) typeInto(s *Server, c *websocket.Conn) {
	e.t.Helper()
	before := yjsRowCount(s, e.page)
	if err := c.Write(context.Background(), websocket.MessageBinary, []byte{frameUpdate, 1, 2, 3}); err != nil {
		e.t.Fatalf("send update: %v", err)
	}
	for i := 0; i < 100 && yjsRowCount(s, e.page) == before; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if yjsRowCount(s, e.page) == before {
		e.t.Fatal("the update was not stored")
	}
}

// closedWith reads until the server closes the socket and returns the code.
func closedWith(c *websocket.Conn) websocket.StatusCode {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// patch is PATCH /api/pages/{id}: from an agent when query is empty, an
// editor's autosave when it carries materialize=1.
func (e *editorTab) patch(query, text string) (int, string) {
	e.t.Helper()
	body := `{"content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`
	req, _ := http.NewRequest("PATCH", e.srv.URL+"/api/pages/"+e.page+query, strings.NewReader(body))
	req.Header.Set("Cookie", e.cookie)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Code string `json:"code"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out.Code
}

// load is what the browser does before it opens an editor: GET the page. It
// returns the generation of the body that came back (docGen).
func (e *editorTab) load() string {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/pages/"+e.page, nil)
	req.Header.Set("Cookie", e.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var p map[string]any
	json.NewDecoder(res.Body).Decode(&p)
	g, ok := p["docGen"].(float64)
	if !ok {
		e.t.Fatalf("the page does not say which document its body is: %v", p["docGen"])
	}
	return strconv.FormatInt(int64(g), 10)
}

func storedContent(s *Server, page string) string {
	var c string
	s.db.QueryRow(`SELECT content FROM pages WHERE id = ?`, page).Scan(&c)
	return c
}

func yjsRowCount(s *Server, page string) int {
	var a, b int
	s.db.QueryRow(`SELECT COUNT(*) FROM yjs_updates WHERE page_id = ?`, page).Scan(&a)
	s.db.QueryRow(`SELECT COUNT(*) FROM yjs_state WHERE page_id = ?`, page).Scan(&b)
	return a + b
}

func genOf(t *testing.T, hello map[string]any) string {
	t.Helper()
	g, ok := hello["gen"].(float64)
	if !ok {
		t.Fatalf("the editor is not told which document it holds: hello = %v", hello)
	}
	return strconv.FormatInt(int64(g), 10)
}

// The reported case, exactly as a 1.1.1 tab and an agent produce it: the human
// types, the agent rewrites the page within the autosave's 1.5 s, and the tab's
// flush (debounced, or on the unmount the 4001 causes) arrives after. It must
// not put the old text back: on 1.1.1 it did, and the rewrite was in no
// revision either.
func TestStaleAutosaveCannotUndoOutsideRewrite(t *testing.T) {
	s, tab, _ := newEditorTab(t)
	if code, _ := tab.patch("", "original"); code != 200 {
		t.Fatalf("seed write: HTTP %d", code)
	}
	c, _, err := tab.open("")
	if err != nil {
		t.Fatalf("editor did not sync: %v", err)
	}
	tab.typeInto(s, c)

	if code, _ := tab.patch("", "agent rewrite"); code != 200 {
		t.Fatalf("rewrite: HTTP %d", code)
	}
	if got := closedWith(c); got != closeReset {
		t.Fatalf("open editor closed with %d, want %d", got, closeReset)
	}
	// What the 1.1.1 frontend sends: materialize=1 and nothing else.
	code, reason := tab.patch("?materialize=1", "original plus a stale draft")
	if got := storedContent(s, tab.page); !strings.Contains(got, "agent rewrite") {
		t.Fatalf("the tab's stale copy replaced the rewrite (HTTP %d): %s", code, got)
	}
	if code != 409 || reason != "doc_replaced" {
		t.Fatalf("stale autosave answered HTTP %d %q, want 409 doc_replaced", code, reason)
	}
	// Refused is not lost: what was typed is in the page history, once, even
	// when a second tab on the same stale document sends the same copy.
	tab.patch("?materialize=1", "original plus a stale draft")
	var kept int
	s.db.QueryRow(`SELECT COUNT(*) FROM page_revisions WHERE page_id = ? AND content LIKE '%stale draft%'`, tab.page).Scan(&kept)
	if kept != 1 {
		t.Fatalf("the refused copy is in the page history %d times, want once", kept)
	}
}

// The generation protocol end to end: the editor learns its document's
// generation, its autosaves are held to it, and a tab that slept through the
// reset is turned away on reconnect instead of pushing its old state into the
// live document.
func TestEditorsAreHeldToTheirDocumentGeneration(t *testing.T) {
	s, tab, _ := newEditorTab(t)
	if code, _ := tab.patch("", "original"); code != 200 {
		t.Fatalf("seed write: HTTP %d", code)
	}
	c, hello, err := tab.open("")
	if err != nil {
		t.Fatalf("editor did not sync: %v", err)
	}
	gen := genOf(t, hello)
	if loaded := tab.load(); loaded != gen {
		t.Fatalf("the body was loaded as generation %s, the live document is %s", loaded, gen)
	}
	tab.typeInto(s, c)
	if code, _ := tab.patch("?materialize=1&gen="+gen, "original typed"); code != 200 {
		t.Fatalf("autosave of the current document: HTTP %d", code)
	}
	// An older frontend (no generation) keeps saving while a live doc exists.
	if code, _ := tab.patch("?materialize=1", "original typed"); code != 200 {
		t.Fatalf("autosave without a generation, live doc present: HTTP %d", code)
	}

	if code, _ := tab.patch("", "agent rewrite"); code != 200 {
		t.Fatalf("rewrite: HTTP %d", code)
	}
	closedWith(c)
	if code, reason := tab.patch("?materialize=1&gen="+gen, "original typed more"); code != 409 || reason != "doc_replaced" {
		t.Fatalf("autosave of the replaced document: HTTP %d %q, content now %s", code, reason, storedContent(s, tab.page))
	}
	if code, _ := tab.patch("?materialize=1&gen=x", "junk"); code != 400 {
		t.Fatalf("a generation that is not a number: HTTP %d", code)
	}

	// The tab whose socket was down during the rewrite comes back.
	stale, _, err := tab.open("?gen=" + gen)
	if got := websocket.CloseStatus(err); got != closeReset {
		t.Fatalf("a reconnect from the replaced document was let in (close %d, err %v)", got, err)
	}
	stale.Write(context.Background(), websocket.MessageBinary, []byte{frameUpdate, 9, 9, 9})
	time.Sleep(100 * time.Millisecond)
	if n := yjsRowCount(s, tab.page); n != 0 {
		t.Fatalf("the stale tab wrote %d rows into the live document", n)
	}
	if !strings.Contains(storedContent(s, tab.page), "agent rewrite") {
		t.Fatalf("rewrite lost: %s", storedContent(s, tab.page))
	}

	// A fresh editor holds the new document and saves normally; so does a
	// reconnect that names it.
	c2, hello2, err := tab.open("")
	if err != nil {
		t.Fatalf("fresh editor: %v", err)
	}
	gen2 := genOf(t, hello2)
	if gen2 == gen {
		t.Fatalf("the rewrite did not start a new generation (%s)", gen)
	}
	if hello2["isNew"] != true {
		t.Fatalf("the fresh editor should seed from the rewrite: %v", hello2)
	}
	tab.typeInto(s, c2)
	if code, _ := tab.patch("?materialize=1&gen="+gen2, "agent rewrite typed"); code != 200 {
		t.Fatalf("autosave of the new document: HTTP %d", code)
	}
	// An editor that knows its copy is stale (it got the 4001) sends it only
	// to have it kept. Never written, even under the current number: an editor
	// that joined in the instant between a rewrite and its reset holds the new
	// number with the old text.
	if code, _ := tab.patch("?materialize=1&gen="+gen2+"&replaced=1", "kept, never written"); code != 409 {
		t.Fatalf("a copy sent only to be kept: HTTP %d", code)
	}
	if strings.Contains(storedContent(s, tab.page), "never written") {
		t.Fatal("a copy sent only to be kept was written")
	}
	var kept int
	s.db.QueryRow(`SELECT COUNT(*) FROM page_revisions WHERE page_id = ? AND content LIKE '%never written%'`, tab.page).Scan(&kept)
	if kept != 1 {
		t.Fatalf("the copy sent to be kept is in the history %d times", kept)
	}
	if _, _, err := tab.open("?gen=" + gen2); err != nil {
		t.Fatalf("a reconnect naming the current generation was refused: %v", err)
	}
}

// Every way of replacing a body from outside the editor has to start a new
// generation, or the autosave of an editor that was open before it is still
// taken. One forgotten path is the whole bug again.
func TestEveryOutsideWriteStartsANewGeneration(t *testing.T) {
	s, tab, u := newEditorTab(t)
	coll := s.makeCollection(t, s.firstWorkspaceOf(t, u.ID), u.ID, "Tasks", `[]`)
	if code, _ := tab.patch("", "original"); code != 200 {
		t.Fatalf("seed write: HTTP %d", code)
	}
	revision := func() string {
		var id string
		if err := s.db.QueryRow(`SELECT id FROM page_revisions WHERE page_id = ? ORDER BY created_at LIMIT 1`, tab.page).Scan(&id); err != nil {
			t.Fatalf("no revision to restore: %v", err)
		}
		return id
	}
	mcp := func(name, args string) func() {
		return func() {
			if _, err := callTool(t, s, u, name, args); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
	p := `"page_id":"` + tab.page + `"`
	writes := []struct {
		name string
		do   func()
	}{
		{"PATCH content", func() { tab.patch("", "rest rewrite") }},
		{"write_content replace", mcp("write_content", `{`+p+`,"markdown":"replaced","mode":"replace"}`)},
		{"write_content append", mcp("write_content", `{`+p+`,"markdown":"appended"}`)},
		{"write_content prepend", mcp("write_content", `{`+p+`,"markdown":"prepended","mode":"prepend"}`)},
		{"upload_file onto the page", mcp("upload_file", `{`+p+`,"file_name":"note.txt","data_base64":"aGVsbG8="}`)},
		{"embed_database", mcp("embed_database", `{`+p+`,"database_id":"`+coll+`"}`)},
		{"restore (REST)", func() {
			req, _ := http.NewRequest("POST", tab.srv.URL+"/api/pages/"+tab.page+"/revisions/"+revision()+"/restore", nil)
			req.Header.Set("Cookie", tab.cookie)
			res, err := http.DefaultClient.Do(req)
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("restore: %v %v", err, res)
			}
			res.Body.Close()
		}},
		{"revisions restore (MCP)", func() {
			mcp("revisions", `{`+p+`,"action":"restore","revision_id":"`+revision()+`"}`)()
		}},
	}
	for _, w := range writes {
		c, hello, err := tab.open("")
		if err != nil {
			t.Fatalf("%s: editor did not sync: %v", w.name, err)
		}
		gen := genOf(t, hello)
		tab.typeInto(s, c)
		w.do()
		if got := closedWith(c); got != closeReset {
			t.Errorf("%s: the open editor was not reset (close %d)", w.name, got)
		}
		before := storedContent(s, tab.page)
		if code, _ := tab.patch("?materialize=1&gen="+gen, "stale copy"); code != 409 {
			t.Errorf("%s: an editor's copy from before it was accepted (HTTP %d)", w.name, code)
		}
		if storedContent(s, tab.page) != before {
			t.Errorf("%s: the stale copy changed the stored content", w.name)
		}
	}
}

// The race this fix found on the way, older than #79 and still in 1.1.1: an
// agent writes twice in a row. The first write closes the open editor, which
// loads the page again (the first write's body) and rejoins; if the second
// write lands in between, the editor used to be told isNew and seeded the new
// live document from the first write's body, so the second write vanished at
// the next keystroke. The editor names the generation of the body it loaded,
// and a body that is already out of date is turned away before it can seed.
func TestEditorCannotSeedFromABodyThatWasReplacedMeanwhile(t *testing.T) {
	s, tab, _ := newEditorTab(t)
	if code, _ := tab.patch("", "first write"); code != 200 {
		t.Fatalf("first write: HTTP %d", code)
	}
	loaded := tab.load() // the editor has the first write's body in hand
	if code, _ := tab.patch("", "second write"); code != 200 {
		t.Fatalf("second write: HTTP %d", code)
	}
	c, hello, err := tab.open("?gen=" + loaded)
	if got := websocket.CloseStatus(err); got != closeReset {
		t.Fatalf("an editor holding the first write's body was let in to seed (close %d, hello %v)", got, hello)
	}
	c.Write(context.Background(), websocket.MessageBinary, []byte{frameUpdate, 7, 7, 7})
	time.Sleep(100 * time.Millisecond)
	if n := yjsRowCount(s, tab.page); n != 0 {
		t.Fatalf("the out-of-date editor wrote %d rows into the live document", n)
	}
	// Loading again gets the second write, and that one is let in.
	if _, hello, err := tab.open("?gen=" + tab.load()); err != nil || hello["isNew"] != true {
		t.Fatalf("an editor holding the current body: err %v, hello %v", err, hello)
	}
}

// --- if_match_docgen: the agent's optimistic concurrency guard --------------
//
// doc_gen numbers the documents (see docgen.go). Until this existed an agent
// could read a page, be told what would change it, and write anyway — the
// editor's stale-copy guard protected the browser and nobody else. These go
// through mcpCall, the way an agent reaches the server, and read the result out
// of the database rather than out of the tool's answer.

func docGenOf(t *testing.T, s *Server, pageID string) int64 {
	t.Helper()
	var gen int64
	if err := s.db.QueryRow(`SELECT doc_gen FROM pages WHERE id = ?`, pageID).Scan(&gen); err != nil {
		t.Fatalf("doc_gen of %s: %v", pageID, err)
	}
	return gen
}

func contentOf(t *testing.T, s *Server, pageID string) string {
	t.Helper()
	var c string
	if err := s.db.QueryRow(`SELECT content FROM pages WHERE id = ?`, pageID).Scan(&c); err != nil {
		t.Fatalf("content of %s: %v", pageID, err)
	}
	return c
}

func revisionCountOf(t *testing.T, s *Server, pageID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM page_revisions WHERE page_id = ?`, pageID).Scan(&n); err != nil {
		t.Fatalf("revisions of %s: %v", pageID, err)
	}
	return n
}

// Zero is a generation like any other. A pointer, not an int, is what makes
// that true: an int field would read 0 as "not given" and let a stale write
// through the one case the guard exists for — a page nobody has rewritten yet.
func TestIfMatchDocGenZeroGuardsAFreshPage(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "docgen-zero@example.test")
	u := &user{ID: uid, Name: "Test"}
	page := s.makePage(t, s.firstWorkspaceOf(t, uid), uid, "", "Guarded", `{}`)
	if gen := docGenOf(t, s, page); gen != 0 {
		t.Fatalf("a fresh page starts at doc_gen %d, want 0", gen)
	}

	if _, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"first","if_match_docgen":0}`); err != nil {
		t.Fatalf("if_match_docgen 0 on a fresh page was refused (%v) — 0 is a real generation, not \"unguarded\"", err)
	}
	if gen := docGenOf(t, s, page); gen != 1 {
		t.Fatalf("after one guarded append doc_gen is %d, want 1", gen)
	}
	before := contentOf(t, s, page)
	beforeRevs := revisionCountOf(t, s, page)

	_, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"second","if_match_docgen":0}`)
	if err == nil {
		t.Fatal("a write naming the superseded doc_gen 0 was accepted")
	}
	if !strings.Contains(err.Error(), "409 Conflict") {
		t.Errorf("refusal does not name 409 Conflict: %v", err)
	}
	if !strings.Contains(err.Error(), "doc_gen is 1 (expected 0)") {
		t.Errorf("refusal does not say what the generation is: %v", err)
	}
	if got := contentOf(t, s, page); got != before {
		t.Errorf("a refused write changed the content:\nbefore %s\nafter  %s", before, got)
	}
	if gen := docGenOf(t, s, page); gen != 1 {
		t.Errorf("a refused write moved doc_gen to %d", gen)
	}
	if got := revisionCountOf(t, s, page); got != beforeRevs {
		t.Errorf("a refused write filed %d revision(s)", got-beforeRevs)
	}
}

// The guard has to hold in all three modes, and leaving it out has to keep the
// old behaviour exactly — including that 0 and "not given" are different.
func TestIfMatchDocGenGuardsEveryWriteMode(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "docgen-modes@example.test")
	u := &user{ID: uid, Name: "Test"}
	page := s.makePage(t, s.firstWorkspaceOf(t, uid), uid, "", "Modes", `{}`)

	// prepend, at the generation the page is at: accepted.
	if _, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"top","mode":"prepend","if_match_docgen":0}`); err != nil {
		t.Fatalf("guarded prepend: %v", err)
	}
	if gen := docGenOf(t, s, page); gen != 1 {
		t.Fatalf("doc_gen after guarded prepend is %d, want 1", gen)
	}
	before := contentOf(t, s, page)
	// prepend again against the old generation: refused, nothing moves.
	if _, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"again","mode":"prepend","if_match_docgen":0}`); err == nil {
		t.Fatal("a stale prepend was accepted")
	}
	if got := contentOf(t, s, page); got != before {
		t.Errorf("a refused prepend changed the content")
	}

	// replace at the current generation: accepted; then stale: refused.
	if _, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"whole body","mode":"replace","if_match_docgen":1}`); err != nil {
		t.Fatalf("guarded replace: %v", err)
	}
	if gen := docGenOf(t, s, page); gen != 2 {
		t.Fatalf("doc_gen after guarded replace is %d, want 2", gen)
	}
	before = contentOf(t, s, page)
	beforeRevs := revisionCountOf(t, s, page)
	if _, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"stale","mode":"replace","if_match_docgen":1}`); err == nil {
		t.Fatal("a stale replace was accepted")
	}
	if got := contentOf(t, s, page); got != before {
		t.Errorf("a refused replace changed the content")
	}
	if gen := docGenOf(t, s, page); gen != 2 {
		t.Errorf("a refused replace moved doc_gen to %d", gen)
	}
	if got := revisionCountOf(t, s, page); got != beforeRevs {
		t.Errorf("a refused replace filed %d revision(s)", got-beforeRevs)
	}

	// No guard at all: the old behaviour, unchanged.
	if _, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"unguarded"}`); err != nil {
		t.Fatalf("an unguarded append was refused: %v", err)
	}
	if gen := docGenOf(t, s, page); gen != 3 {
		t.Errorf("doc_gen after an unguarded write is %d, want 3", gen)
	}
}

func TestIfMatchDocGenRefusesNegativeGenerations(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "docgen-negative@example.test")
	u := &user{ID: uid, Name: "Test"}
	page := s.makePage(t, s.firstWorkspaceOf(t, uid), uid, "", "Negative", `{}`)
	before := contentOf(t, s, page)

	_, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"x","if_match_docgen":-1}`)
	if err == nil || !strings.Contains(err.Error(), "if_match_docgen must be >= 0") {
		t.Fatalf("if_match_docgen -1 gave %v, want \"if_match_docgen must be >= 0\"", err)
	}
	if got := contentOf(t, s, page); got != before {
		t.Error("a refused negative generation still wrote")
	}
	if gen := docGenOf(t, s, page); gen != 0 {
		t.Errorf("doc_gen is %d after a refused write, want 0", gen)
	}
}

// get_permissions is where an agent learns the number, so it has to be the
// number the guard actually compares — not an approximation of it.
func TestGetPermissionsReportsTheDocGenTheGuardUses(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "docgen-perm@example.test")
	u := &user{ID: uid, Name: "Test"}
	page := s.makePage(t, s.firstWorkspaceOf(t, uid), uid, "", "Perms", `{}`)

	out, err := callTool(t, s, u, "get_permissions", `{"page_id":"`+page+`"}`)
	if err != nil {
		t.Fatalf("get_permissions: %v", err)
	}
	var perms map[string]any
	if err := json.Unmarshal([]byte(out), &perms); err != nil {
		t.Fatalf("get_permissions is not JSON: %v\n%s", err, out)
	}
	if perms["doc_gen"] != float64(0) {
		t.Fatalf("doc_gen is %v on a fresh page, want 0", perms["doc_gen"])
	}

	if _, err := callTool(t, s, u, "write_content", `{"page_id":"`+page+`","markdown":"body"}`); err != nil {
		t.Fatalf("append: %v", err)
	}
	out, err = callTool(t, s, u, "get_permissions", `{"page_id":"`+page+`"}`)
	if err != nil {
		t.Fatalf("get_permissions: %v", err)
	}
	if err := json.Unmarshal([]byte(out), &perms); err != nil {
		t.Fatalf("get_permissions is not JSON: %v", err)
	}
	gen := docGenOf(t, s, page)
	if perms["doc_gen"] != float64(gen) {
		t.Fatalf("get_permissions says doc_gen %v, the page is at %d", perms["doc_gen"], gen)
	}
	// The number it handed out is the one that lets the next write through.
	if _, err := callTool(t, s, u, "write_content",
		`{"page_id":"`+page+`","markdown":"more","if_match_docgen":`+strconv.FormatInt(int64(perms["doc_gen"].(float64)), 10)+`}`); err != nil {
		t.Fatalf("the doc_gen get_permissions reported was refused: %v", err)
	}
}
