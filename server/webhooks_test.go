package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mustIP fails the test rather than silently checking a nil address, which
// blockedIP would wave through.
func mustIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("%q is not an IP — the test itself is wrong", s)
	}
	return ip
}

// A webhook URL is a text field that makes the server call an address somebody
// typed. That is the shape of an SSRF hole, and the reason it is dangerous here
// is that the caller is INSIDE the network: cloud metadata services, the
// hypervisor, the router, anything the instance can reach and the internet
// cannot.
//
// safeDial is the guard (see ingest.go). These assert the addresses it must
// refuse — at the resolved IP, not by string matching, so an attacker cannot
// walk around it with a hostname that resolves inward.
func TestWebhookRefusesInternalAddresses(t *testing.T) {
	for _, ip := range []string{
		"169.254.169.254", // cloud metadata, the classic target
		"127.0.0.1",       // the instance itself
		"10.0.0.1",        // private network
		"172.16.0.1",      // private network
		"192.168.1.1",     // the router
		"0.0.0.0",         // unspecified
		"::1",             // loopback, v6
		"fd00::1",         // unique local, v6
		"224.0.0.1",       // multicast
	} {
		if !blockedIP(mustIP(t, ip)) {
			t.Errorf("%s is not blocked — a webhook could reach it from inside the network", ip)
		}
	}
	// Public addresses have to stay reachable, or the feature does nothing.
	for _, ip := range []string{"1.1.1.1", "93.184.216.34", "2606:4700::1111"} {
		if blockedIP(mustIP(t, ip)) {
			t.Errorf("%s is blocked, but a webhook has to be able to reach the internet", ip)
		}
	}
}

// The URL check is about shape, not safety — safety happens per resolved IP at
// delivery time. It still has to refuse the obvious.
func TestWebhookURLValidation(t *testing.T) {
	bad := []string{
		"", "not a url", "ftp://example.com", "file:///etc/passwd",
		"javascript:alert(1)", "https://", "gopher://example.com",
	}
	for _, u := range bad {
		if err := validWebhookURL(u); err == nil {
			t.Errorf("%q was accepted as a webhook URL", u)
		}
	}
	for _, u := range []string{"https://example.com/hook", "http://example.com:9000/x?y=1"} {
		if err := validWebhookURL(u); err != nil {
			t.Errorf("%q should be accepted: %v", u, err)
		}
	}
}

// End to end against a real receiver: the payload arrives, it is signed with
// the secret, and it names the page WITHOUT carrying its body.
func TestWebhookDeliversASignedPayload(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	var gotSig string
	done := make(chan struct{}, 1)

	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		gotSig = r.Header.Get("X-Salt-Signature")
		select {
		case done <- struct{}{}:
		default:
		}
		w.WriteHeader(200)
	}))
	defer recv.Close()

	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := newID()
	s.db.Exec(`INSERT INTO workspaces (id, name, created_at) VALUES (?, 'W', ?)`, ws, now())
	pid := newID()
	s.db.Exec(`INSERT INTO pages (id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility)
		VALUES (?, 'Secret plans', '[{"type":"paragraph"}]', 1, ?, ?, ?, ?, 'workspace')`,
		pid, now(), now(), ws, uid)

	secret := "topsecret"
	s.db.Exec(`INSERT INTO webhooks (id, url, secret, events, active, created_at)
		VALUES (?, ?, ?, 'page.updated', 1, ?)`, newID(), recv.URL, secret, now())

	// The receiver is on 127.0.0.1, which safeDial refuses on purpose, so the
	// test lends the server an ordinary transport. Everything else — signing,
	// headers, the payload — is the real path.
	s.webhookTransport = http.DefaultTransport
	s.fireWebhook("page.updated", pid)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery within three seconds")
	}

	mu.Lock()
	defer mu.Unlock()

	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, gotBody)
	}
	if body["event"] != "page.updated" {
		t.Errorf("event is %v", body["event"])
	}
	page, _ := body["page"].(map[string]any)
	if page["id"] != pid {
		t.Errorf("page id is %v, want %s", page["id"], pid)
	}
	if page["title"] != "Secret plans" {
		t.Errorf("title is %v", page["title"])
	}
	// The line that matters: a webhook names a page, it does not export it.
	if strings.Contains(string(gotBody), "paragraph") {
		t.Errorf("the payload carries page content:\n%s", gotBody)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature is %q, want %q — a receiver cannot tell our call from anyone else's", gotSig, want)
	}
}

// A hook that is not subscribed to the event must stay quiet, and one that is
// switched off must stay quiet whatever it is subscribed to.
func TestWebhookOnlyFiresForSubscribedEvents(t *testing.T) {
	var hits int
	var mu sync.Mutex
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer recv.Close()

	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := newID()
	s.db.Exec(`INSERT INTO workspaces (id, name, created_at) VALUES (?, 'W', ?)`, ws, now())
	pid := newID()
	s.db.Exec(`INSERT INTO pages (id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility)
		VALUES (?, 'P', '[]', 1, ?, ?, ?, ?, 'workspace')`, pid, now(), now(), ws, uid)

	s.webhookTransport = http.DefaultTransport
	s.db.Exec(`INSERT INTO webhooks (id, url, secret, events, active, created_at)
		VALUES (?, ?, 's', 'page.created', 1, ?)`, newID(), recv.URL, now())
	s.db.Exec(`INSERT INTO webhooks (id, url, secret, events, active, created_at)
		VALUES (?, ?, 's', 'page.updated', 0, ?)`, newID(), recv.URL, now())

	s.fireWebhook("page.updated", pid)
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("%d deliveries — one hook is subscribed to another event, the other is switched off", hits)
	}
}

// An unknown event name must not become a delivery, or a typo in a call site
// turns into traffic nobody asked for.
func TestWebhookIgnoresUnknownEvents(t *testing.T) {
	s := testServer(t)
	s.db.Exec(`INSERT INTO webhooks (id, url, secret, events, active, created_at)
		VALUES (?, 'https://example.com', 's', 'page.updated', 1, ?)`, newID(), now())
	// Would panic or deliver if the guard were missing; asserting it returns.
	s.fireWebhook("page.exploded", newID())
}

// The guard has to be IN the delivery path, not merely present in the package.
//
// This is the test that would have caught a copy-paste of the plain
// http.DefaultClient into deliverWebhook: with no transport lent to the server,
// a receiver on 127.0.0.1 must NOT be reached, and the failure must be recorded
// where an admin can see it.
func TestWebhookGuardIsInTheDeliveryPath(t *testing.T) {
	var hits int
	var mu sync.Mutex
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer recv.Close()

	s := testServer(t)
	uid, _ := signedIn(t, s, "a@example.com")
	ws := newID()
	s.db.Exec(`INSERT INTO workspaces (id, name, created_at) VALUES (?, 'W', ?)`, ws, now())
	pid := newID()
	s.db.Exec(`INSERT INTO pages (id, title, content, position, created_at, updated_at, workspace_id, owner_id, visibility)
		VALUES (?, 'P', '[]', 1, ?, ?, ?, ?, 'workspace')`, pid, now(), now(), ws, uid)

	hookID := newID()
	s.db.Exec(`INSERT INTO webhooks (id, url, secret, events, active, created_at)
		VALUES (?, ?, 's', 'page.updated', 1, ?)`, hookID, recv.URL, now())

	// Deliberately NOT lending a transport: this is the production path.
	s.fireWebhook("page.updated", pid)
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 0 {
		t.Errorf("%d deliveries reached a loopback address — the SSRF guard is not in the delivery path", got)
	}

	var status string
	s.db.QueryRow(`SELECT COALESCE(last_status,'') FROM webhooks WHERE id = ?`, hookID).Scan(&status)
	if !strings.Contains(status, "failed") {
		t.Errorf("last_status is %q — a refused delivery has to be visible to an admin", status)
	}
}

// --- row.* events -----------------------------------------------------------
//
// A board is where the work happens, and it used to be the one thing an
// integration could not hear: `row.created`, `row.updated` and `row.deleted`
// exist so a receiver can react to a card moving without polling the whole
// database. These drive the real call sites — REST and MCP — because an event
// that is only fired from a test proves nothing about the paths people use.

// hookSink is a receiver that keeps every delivery and hands them out over a
// channel, so a test can wait for the third one without sleeping.
type hookSink struct {
	mu  sync.Mutex
	got []hookCall
	ch  chan hookCall
}

type hookCall struct {
	body []byte
	sig  string
}

func newHookSink() *hookSink {
	return &hookSink{ch: make(chan hookCall, 64)}
}

func (h *hookSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c := hookCall{body: body, sig: r.Header.Get("X-Salt-Signature")}
	h.mu.Lock()
	h.got = append(h.got, c)
	h.mu.Unlock()
	select {
	case h.ch <- c:
	default:
	}
	w.WriteHeader(200)
}

func (h *hookSink) next(t *testing.T, d time.Duration) hookCall {
	t.Helper()
	select {
	case c := <-h.ch:
		return c
	case <-time.After(d):
		t.Fatalf("no delivery within %s", d)
		return hookCall{}
	}
}

// silent waits out the window and reports whether nothing arrived. It drains
// the channel rather than counting, so a delivery a previous step already
// consumed cannot be mistaken for a new one.
func (h *hookSink) silent(d time.Duration) bool {
	time.Sleep(d)
	select {
	case <-h.ch:
		return false
	default:
		return true
	}
}

func (h *hookSink) payload(t *testing.T, c hookCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.body, &m); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, c.body)
	}
	return m
}

// hookFor subscribes a sink to exactly one event on a normal test server.
func hookFor(t *testing.T, s *Server, sink *hookSink, events, secret string) *httptest.Server {
	t.Helper()
	recv := httptest.NewServer(sink)
	t.Cleanup(recv.Close)
	s.webhookTransport = http.DefaultTransport
	if _, err := s.db.Exec(`INSERT INTO webhooks (id, url, secret, events, active, created_at)
		VALUES (?, ?, ?, ?, 1, ?)`, newID(), recv.URL, secret, events, now()); err != nil {
		t.Fatalf("insert webhook: %v", err)
	}
	return recv
}

func signedBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// A row created through REST arrives as row.created, it names its database, and
// it is signed like every other delivery.
func TestRowCreatedWebhookNamesItsCollection(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "rowcreated@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	coll := s.makeCollection(t, ws, uid, "Tasks", `[{"id":"status","name":"Status","type":"text"}]`)
	sink := newHookSink()
	const secret = "row-secret"
	hookFor(t, s, sink, "row.created", secret)

	rec := requestAs(t, s, cookie, "POST", "/api/pages", `{"parentId":"`+coll+`","title":"Ship it"}`)
	if rec.Code != 200 {
		t.Fatalf("create row: HTTP %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create answer is not JSON: %v", err)
	}
	rowID, _ := created["id"].(string)

	c := sink.next(t, 3*time.Second)
	if got := c.sig; got != signedBody(secret, c.body) {
		t.Errorf("signature is %q, want the HMAC of the body", got)
	}
	p := sink.payload(t, c)
	if p["event"] != "row.created" {
		t.Errorf("event is %v", p["event"])
	}
	if p["collectionId"] != coll {
		t.Errorf("collectionId is %v, want %s", p["collectionId"], coll)
	}
	row, _ := p["row"].(map[string]any)
	if row["id"] != rowID {
		t.Errorf("row id is %v, want %s", row["id"], rowID)
	}
	if row["title"] != "Ship it" {
		t.Errorf("row title is %v", row["title"])
	}
	if row["path"] != "/p/"+rowID {
		t.Errorf("row path is %v", row["path"])
	}
	// The rule the page events follow applies here too: name it, don't export it.
	if strings.Contains(string(c.body), "paragraph") {
		t.Errorf("the payload carries the row's body:\n%s", c.body)
	}
}

// A document is not a row. Nobody asked for row events on the page tree, and a
// receiver filtering on a database must not be woken by an unrelated page.
func TestRowEventsStayQuietForANormalPage(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "rowquiet@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	sink := newHookSink()
	hookFor(t, s, sink, "row.created,row.updated,row.deleted", "s")

	rec := requestAs(t, s, cookie, "POST", "/api/pages", `{"title":"Loose document","workspaceId":"`+ws+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create page: HTTP %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	pageID, _ := created["id"].(string)

	if !sink.silent(500 * time.Millisecond) {
		t.Fatal("creating a document fired a row.* event")
	}

	// Nor does an update or a trash of that same page.
	if rec := requestAs(t, s, cookie, "PATCH", "/api/pages/"+pageID, `{"title":"Renamed"}`); rec.Code != 200 {
		t.Fatalf("update page: HTTP %d", rec.Code)
	}
	if rec := requestAs(t, s, cookie, "DELETE", "/api/pages/"+pageID, ""); rec.Code != 200 {
		t.Fatalf("trash page: HTTP %d", rec.Code)
	}
	if !sink.silent(500 * time.Millisecond) {
		t.Fatal("updating or trashing a document fired a row.* event")
	}
}

// row.updated says which properties moved — that is the whole difference
// between "something changed, fetch the row" and being able to act on it.
func TestRowUpdatedWebhookNamesChangedProperties(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "rowupdated@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	coll := s.makeCollection(t, ws, uid, "Tasks", `[{"id":"status","name":"Status","type":"text"}]`)
	row := s.makeRow(t, ws, uid, coll, "Card", `{}`)
	sink := newHookSink()
	hookFor(t, s, sink, "row.updated", "s")

	if _, err := callTool(t, s, u, "set_properties",
		`{"page_id":"`+row+`","properties":{"status":"Done"}}`); err != nil {
		t.Fatalf("set_properties: %v", err)
	}
	p := sink.payload(t, sink.next(t, 3*time.Second))
	if p["event"] != "row.updated" {
		t.Errorf("event is %v", p["event"])
	}
	if p["collectionId"] != coll {
		t.Errorf("collectionId is %v, want %s", p["collectionId"], coll)
	}
	changed, _ := p["changedProperties"].([]any)
	if len(changed) != 1 || changed[0] != "status" {
		t.Errorf("changedProperties is %v, want [status]", p["changedProperties"])
	}
}

// The browser's own path (propsPatch from a board) has to report the same way,
// or a receiver sees agent changes and not human ones.
func TestRowUpdatedWebhookFromTheBrowser(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "rowupdated-rest@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	coll := s.makeCollection(t, ws, uid, "Tasks", `[{"id":"status","name":"Status","type":"text"}]`)
	row := s.makeRow(t, ws, uid, coll, "Card", `{}`)
	sink := newHookSink()
	hookFor(t, s, sink, "row.updated", "s")

	rec := requestAs(t, s, cookie, "PATCH", "/api/pages/"+row, `{"propsPatch":{"status":"Doing"}}`)
	if rec.Code != 200 {
		t.Fatalf("propsPatch: HTTP %d %s", rec.Code, rec.Body.String())
	}
	p := sink.payload(t, sink.next(t, 3*time.Second))
	if p["event"] != "row.updated" {
		t.Errorf("event is %v", p["event"])
	}
	changed, _ := p["changedProperties"].([]any)
	if len(changed) != 1 || changed[0] != "status" {
		t.Errorf("changedProperties is %v, want [status]", p["changedProperties"])
	}
}

// The hard delete is the case that cannot be got right by reading after the
// fact: `DELETE FROM pages` takes the row with it, so the collection id has to
// be captured before the commit. Trashing and deleting for good both report.
func TestRowDeletedWebhookSurvivesAHardDelete(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "rowdeleted@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	coll := s.makeCollection(t, ws, uid, "Tasks", `[{"id":"status","name":"Status","type":"text"}]`)
	trashed := s.makeRow(t, ws, uid, coll, "Sent to trash", `{}`)
	gone := s.makeRow(t, ws, uid, coll, "Deleted for good", `{}`)
	sink := newHookSink()
	hookFor(t, s, sink, "row.deleted", "s")

	if rec := requestAs(t, s, cookie, "DELETE", "/api/pages/"+trashed, ""); rec.Code != 200 {
		t.Fatalf("trash row: HTTP %d %s", rec.Code, rec.Body.String())
	}
	p := sink.payload(t, sink.next(t, 3*time.Second))
	if p["event"] != "row.deleted" {
		t.Errorf("event is %v", p["event"])
	}
	if p["collectionId"] != coll {
		t.Errorf("collectionId is %v, want %s", p["collectionId"], coll)
	}
	row, _ := p["row"].(map[string]any)
	if row["id"] != trashed || row["title"] != "Sent to trash" {
		t.Errorf("trashed row is %v", row)
	}

	if rec := requestAs(t, s, cookie, "DELETE", "/api/pages/"+gone+"?permanent=1", ""); rec.Code != 200 {
		t.Fatalf("delete row for good: HTTP %d %s", rec.Code, rec.Body.String())
	}
	p = sink.payload(t, sink.next(t, 3*time.Second))
	if p["collectionId"] != coll {
		t.Errorf("collectionId is %v after a hard delete — the metadata was read too late", p["collectionId"])
	}
	row, _ = p["row"].(map[string]any)
	if row["id"] != gone {
		t.Errorf("row id is %v, want %s", row["id"], gone)
	}
	if row["title"] != "Deleted for good" {
		t.Errorf("title is %v — a hard delete lost the row's title", row["title"])
	}
}

// An agent trashing a page is the path that used to be silent for every
// receiver, page events included. It reports both families now.
func TestAgentTrashFiresPageAndRowEvents(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "agenttrash@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	coll := s.makeCollection(t, ws, uid, "Tasks", `[{"id":"status","name":"Status","type":"text"}]`)
	row := s.makeRow(t, ws, uid, coll, "Agent trash", `{}`)
	sink := newHookSink()
	hookFor(t, s, sink, "page.trashed,row.deleted", "s")

	if _, err := callTool(t, s, u, "set_trashed", `{"page_id":"`+row+`","trashed":true}`); err != nil {
		t.Fatalf("set_trashed: %v", err)
	}
	seen := map[string]map[string]any{}
	for i := 0; i < 2; i++ {
		p := sink.payload(t, sink.next(t, 3*time.Second))
		ev, _ := p["event"].(string)
		seen[ev] = p
	}
	if _, ok := seen["page.trashed"]; !ok {
		t.Error("set_trashed fired no page.trashed")
	}
	m, ok := seen["row.deleted"]
	if !ok {
		t.Fatal("set_trashed fired no row.deleted")
	}
	if m["collectionId"] != coll {
		t.Errorf("row.deleted collectionId is %v, want %s", m["collectionId"], coll)
	}
}

// create_rows is a batch, and a receiver watching a database expects one
// message per row, not one for the batch.
func TestCreateRowsFiresOneRowEventPerRow(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "createrows@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	coll := s.makeCollection(t, ws, uid, "Tasks", `[{"id":"status","name":"Status","type":"text"}]`)
	sink := newHookSink()
	hookFor(t, s, sink, "row.created", "s")

	if _, err := callTool(t, s, u, "create_rows",
		`{"page_id":"`+coll+`","rows":[{"title":"A"},{"title":"B"}]}`); err != nil {
		t.Fatalf("create_rows: %v", err)
	}
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		p := sink.payload(t, sink.next(t, 3*time.Second))
		if p["event"] != "row.created" {
			t.Errorf("event is %v", p["event"])
		}
		if p["collectionId"] != coll {
			t.Errorf("collectionId is %v, want %s", p["collectionId"], coll)
		}
		row, _ := p["row"].(map[string]any)
		id, _ := row["id"].(string)
		ids[id] = true
	}
	if len(ids) != 2 {
		t.Errorf("two rows produced %d distinct row ids", len(ids))
	}
	if !sink.silent(300 * time.Millisecond) {
		t.Error("create_rows of two rows delivered more than two row.created events")
	}
}

// The UI, the catalogue and the map must name the same six events. The dialog
// is only a view of webhookEvents, and a worker can add one without the other.
func TestWebhookEventCatalogueMatchesTheDialog(t *testing.T) {
	for _, ev := range []string{"page.created", "page.updated", "page.trashed", "row.created", "row.updated", "row.deleted"} {
		if !webhookEvents[ev] {
			t.Errorf("%s is not an accepted webhook event", ev)
		}
	}
	if len(webhookEvents) != 6 {
		t.Errorf("there are %d webhook events, want 6", len(webhookEvents))
	}
}
