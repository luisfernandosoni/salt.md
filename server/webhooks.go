package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Outbound webhooks (W114).
//
// The gap this fills: salt.md had nothing that reaches OUT. `/api/events` is a
// server-sent stream, which needs a signed-in client holding a connection open
// — fine for the app's own tabs, useless for Zapier, Make, n8n or a script on
// somebody's server. Without an outbound call, every integration has to poll.
//
// That single missing piece is why salt.md looked like it had no integration
// story next to Notion's directory of connectors. Notion's connectors are not
// plugins running inside Notion; they are outside services calling an API and
// being called back. The API exists here. The call back did not.
//
// Three decisions worth knowing before changing anything:
//
// **The payload names a page, it does not carry it.** id, title, workspace,
// URL — never the body. A webhook URL is typed once by an admin and then sends
// forever to a host nobody re-checks; making it a content pipe would turn one
// careless paste into a standing export of everything anybody writes. A
// receiver that is allowed to read the page can fetch it with its own token.
//
// **Delivery goes through safeDial**, the same guard the bulk importer uses
// (see ingest.go). Without it, "webhook URL" is a text field that makes the
// server fetch any address an admin can name — including 169.254.169.254 and
// whatever else sits in the private network around it. This is the classic way
// a harmless-looking feature becomes an SSRF hole.
//
// **Every delivery is signed.** The receiver cannot otherwise tell our call
// from anybody else's POST to the same URL.

const webhookTimeout = 10 * time.Second

// webhookEvents are the events a hook can subscribe to. Deliberately few: each
// one has to be fired from real call sites, and an event that is documented but
// never arrives is worse than one that does not exist.
//
// The three page.* events name a page; the three row.* events name a row of a
// database (which is a page too) and carry its collection, so a receiver can
// act on "a card moved to Done" without diffing a whole board.
var webhookEvents = map[string]bool{
	"page.created": true,
	"page.updated": true,
	"page.trashed": true,
	"row.created":  true,
	"row.updated":  true,
	"row.deleted":  true,
}

type webhook struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	Events     string `json:"events"`
	Active     bool   `json:"active"`
	CreatedAt  string `json:"createdAt"`
	LastStatus string `json:"lastStatus"`
	LastAt     string `json:"lastAt"`
	// Secret is returned ONCE, when the hook is created. After that it is
	// write-only, like an API token — the receiver has it, and we cannot show
	// it again without making the audit trail meaningless.
	Secret string `json:"secret,omitempty"`
}

// validWebhookURL keeps obvious nonsense out of the column. It does NOT decide
// whether the address is safe to call — safeDial does that at delivery time,
// per resolved IP, because DNS can say something different by then.
func validWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("that does not look like a URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("a webhook URL has to start with https:// or http://")
	}
	if u.Host == "" {
		return fmt.Errorf("the URL has no host")
	}
	if len(raw) > 500 {
		return fmt.Errorf("that URL is too long")
	}
	return nil
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, url, events, active, created_at,
		COALESCE(last_status,''), COALESCE(last_at,'') FROM webhooks ORDER BY created_at`)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []webhook{}
	for rows.Next() {
		var h webhook
		var active int
		if rows.Scan(&h.ID, &h.URL, &h.Events, &active, &h.CreatedAt, &h.LastStatus, &h.LastAt) == nil {
			h.Active = active != 0
			out = append(out, h)
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpError(w, 400, "invalid JSON")
		return
	}
	if err := validWebhookURL(body.URL); err != nil {
		httpErrorCode(w, 400, "webhook_bad_url", err.Error())
		return
	}
	var events []string
	for _, e := range body.Events {
		e = strings.TrimSpace(e)
		if webhookEvents[e] {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		httpErrorCode(w, 400, "webhook_no_events",
			"Pick at least one event: page.created, page.updated, page.trashed, row.created, row.updated or row.deleted.")
		return
	}
	secret := newID() + newID()
	id := newID()
	if _, err := s.db.Exec(`INSERT INTO webhooks (id, url, secret, events, active, created_at)
		VALUES (?, ?, ?, ?, 1, ?)`, id, strings.TrimSpace(body.URL), secret, strings.Join(events, ","), now()); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.audit("human", requestUser(r).ID, requestUser(r).Name, "create_webhook", "", "", body.URL)
	writeJSON(w, webhook{ID: id, URL: body.URL, Events: strings.Join(events, ","),
		Active: true, CreatedAt: now(), Secret: secret})
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var target string
	s.db.QueryRow(`SELECT url FROM webhooks WHERE id = ?`, id).Scan(&target)
	if _, err := s.db.Exec(`DELETE FROM webhooks WHERE id = ?`, id); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.audit("human", requestUser(r).ID, requestUser(r).Name, "delete_webhook", "", "", target)
	writeJSON(w, map[string]any{"ok": true})
}

// fireWebhook tells every subscribed hook that something happened to a page.
//
// Never blocks the caller and never fails a write: a save that succeeded must
// not be reported as an error because somebody's endpoint is down. Failures go
// to the log and to last_status, where an admin can see them.
func (s *Server) fireWebhook(event, pageID string) {
	if !webhookEvents[event] {
		return
	}
	targets := s.webhookTargets(event)
	if len(targets) == 0 {
		return
	}

	var title, wsID string
	s.db.QueryRow(`SELECT title, workspace_id FROM pages WHERE id = ?`, pageID).Scan(&title, &wsID)

	payload, err := json.Marshal(map[string]any{
		"event": event,
		"at":    now(),
		"page": map[string]any{
			"id":          pageID,
			"title":       title,
			"workspaceId": wsID,
			"path":        "/p/" + pageID,
		},
	})
	if err != nil {
		return
	}

	// A page that is going away takes its pending update with it. Delivering
	// page.updated AFTER page.trashed would describe a page the receiver has
	// already been told is gone, in the wrong order, from a payload captured
	// before the delete.
	if event == "page.trashed" {
		s.cancelDebouncedWebhooks(pageID)
	}

	// Debounce only page.updated. The other five events describe one act each
	// (created, trashed, a row's field moving), and delaying them would make an
	// integration react late to something that already happened once.
	if event == "page.updated" {
		if wait := s.effectiveWebhookDebounce(); wait > 0 {
			for _, t := range targets {
				s.scheduleDebouncedWebhook(t, pageID, payload, wait)
			}
			return
		}
	}
	for _, t := range targets {
		go s.deliverWebhook(t.id, t.url, t.secret, payload)
	}
}

// debouncedWebhook is one page.updated waiting out its window: the timer that
// will send it and the LATEST payload for that page. Keeping the payload is the
// whole point — an agent that saves six times in ten seconds would otherwise
// have the FIRST of the six delivered, describing a state that no longer exists.
type debouncedWebhook struct {
	timer   *time.Timer
	target  webhookTarget
	payload []byte
}

// effectiveWebhookDebounce is the window page.updated waits for another save of
// the same page, in which the two become one delivery.
//
// A server that was handed a transport is a test: it wants the delivery, not the
// window, unless the test set one on purpose (webhookDebounceFixed).
func (s *Server) effectiveWebhookDebounce() time.Duration {
	if s.webhookTransport != nil && !s.webhookDebounceFixed {
		return 0
	}
	return s.webhookDebounce
}

// effectiveWebhookRetryDelay is how long a failed delivery waits before its one
// retry. Five seconds against a real endpoint; fifty milliseconds in tests, so a
// retry is provable without a test that takes five seconds.
func (s *Server) effectiveWebhookRetryDelay() time.Duration {
	if s.webhookRetryDelay > 0 {
		return s.webhookRetryDelay
	}
	if s.webhookTransport != nil {
		return 50 * time.Millisecond
	}
	return 5 * time.Second
}

// webhookKey names one pending delivery: a hook and the page it is about. The
// pair, not the page alone, because two hooks subscribe to the same page and
// each needs its own coalescing window.
func webhookKey(targetID, pageID string) string { return targetID + ":" + pageID }

// scheduleDebouncedWebhook arms (or re-arms) the window for one hook and page,
// keeping the newest payload.
func (s *Server) scheduleDebouncedWebhook(t webhookTarget, pageID string, payload []byte, wait time.Duration) {
	key := webhookKey(t.id, pageID)
	pending := &debouncedWebhook{target: t, payload: payload}
	pending.timer = time.AfterFunc(wait, func() {
		s.webhookMu.Lock()
		// Only the entry this timer owns may remove itself: a newer save may
		// have replaced it after Stop and before this goroutine ran.
		if s.webhookTimers[key] == pending {
			delete(s.webhookTimers, key)
		}
		s.webhookMu.Unlock()
		s.deliverWebhook(t.id, t.url, t.secret, payload)
	})
	s.webhookMu.Lock()
	if old := s.webhookTimers[key]; old != nil {
		old.timer.Stop()
	}
	s.webhookTimers[key] = pending
	s.webhookMu.Unlock()
}

// cancelDebouncedWebhooks drops every pending update for a page, whatever hooks
// were waiting for it.
func (s *Server) cancelDebouncedWebhooks(pageID string) {
	suffix := ":" + pageID
	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	for key, pending := range s.webhookTimers {
		if strings.HasSuffix(key, suffix) {
			pending.timer.Stop()
			delete(s.webhookTimers, key)
		}
	}
}

// stopDebouncedWebhooks stops every pending timer. Called from Close, or a
// shutdown would leave timers firing into a closed database.
func (s *Server) stopDebouncedWebhooks() {
	s.webhookMu.Lock()
	defer s.webhookMu.Unlock()
	for key, pending := range s.webhookTimers {
		pending.timer.Stop()
		delete(s.webhookTimers, key)
	}
}

// webhookTarget is one active hook that asked for a given event.
type webhookTarget struct{ id, url, secret string }

// webhookTargets reads the active hooks subscribed to an event. The rows are
// drained before returning — there is one database connection, so a caller
// that queried again with a cursor still open would wait forever.
func (s *Server) webhookTargets(event string) []webhookTarget {
	rows, err := s.db.Query(`SELECT id, url, secret, events FROM webhooks WHERE active = 1`)
	if err != nil {
		return nil
	}
	var targets []webhookTarget
	for rows.Next() {
		var t webhookTarget
		var events string
		if rows.Scan(&t.id, &t.url, &t.secret, &events) != nil {
			continue
		}
		for _, e := range strings.Split(events, ",") {
			if strings.TrimSpace(e) == event {
				targets = append(targets, t)
				break
			}
		}
	}
	rows.Close()
	return targets
}

// rowWebhookMeta is what a row.* event has to know about a row, captured while
// the row still exists. A hard delete removes the pages row, so anything read
// after the commit would come back empty — and the collection id would be lost
// with it.
type rowWebhookMeta struct {
	RowID        string
	CollectionID string
	Title        string
	WorkspaceID  string
	Changed      []string
}

// lookupCollectionRow reports whether rowID is a row of a database, and if so
// what the event needs to name it. A page that is not a row (a document, a
// collection itself) is not a row event, whatever else happens to it.
//
// Both queries run outside any transaction, and the cursor of the first is
// closed by QueryRow before the second — one connection.
func (s *Server) lookupCollectionRow(rowID string) (rowWebhookMeta, bool) {
	var parentID, title, wsID string
	if s.db.QueryRow(`SELECT COALESCE(parent_id, ''), title, workspace_id FROM pages WHERE id = ?`, rowID).
		Scan(&parentID, &title, &wsID) != nil {
		return rowWebhookMeta{}, false
	}
	if parentID == "" {
		return rowWebhookMeta{}, false
	}
	var isCollection int
	if s.db.QueryRow(`SELECT COUNT(*) FROM collections WHERE page_id = ?`, parentID).Scan(&isCollection) != nil || isCollection == 0 {
		return rowWebhookMeta{}, false
	}
	return rowWebhookMeta{RowID: rowID, CollectionID: parentID, Title: title, WorkspaceID: wsID}, true
}

// fireRowWebhookMeta emits a row.* event for metadata that was captured
// earlier. The payload never carries the row's body, the same rule the page
// events follow: a webhook names an object, it does not export it.
func (s *Server) fireRowWebhookMeta(event string, meta rowWebhookMeta) {
	if !webhookEvents[event] || meta.RowID == "" {
		return
	}
	targets := s.webhookTargets(event)
	if len(targets) == 0 {
		return
	}
	changed := append([]string{}, meta.Changed...)
	// Sorted: the caller collects keys from a Go map, and a payload whose field
	// order changes between two identical events is a receiver's problem for
	// free.
	sort.Strings(changed)
	payload, err := json.Marshal(map[string]any{
		"event":        event,
		"at":           now(),
		"collectionId": meta.CollectionID,
		"row": map[string]any{
			"id":          meta.RowID,
			"title":       meta.Title,
			"workspaceId": meta.WorkspaceID,
			"path":        "/p/" + meta.RowID,
		},
		"changedProperties": changed,
	})
	if err != nil {
		return
	}
	for _, t := range targets {
		go s.deliverWebhook(t.id, t.url, t.secret, payload)
	}
}

// fireRowWebhook is the convenience form for call sites that still have the
// row: it looks the collection up and stays silent for a page that is not one.
func (s *Server) fireRowWebhook(event, rowID string, changed []string) {
	meta, ok := s.lookupCollectionRow(rowID)
	if !ok {
		return
	}
	meta.Changed = changed
	s.fireRowWebhookMeta(event, meta)
}

// deliverWebhook sends one event, retrying once when the failure can be
// transient (a transport error, 429 or a 5xx). A 4xx is the receiver saying the
// request itself is wrong; sending it again would only be noise.
func (s *Server) deliverWebhook(id, target, secret string, payload []byte) {
	defer func() {
		// A panic in a background goroutine takes the whole server with it, and
		// a webhook is the last thing that should be able to do that.
		if rec := recover(); rec != nil {
			log.Printf("webhook %s: recovered from %v", id, rec)
		}
	}()

	status, err := s.postOnce(id, target, secret, payload)
	// Record what the first attempt did before waiting for the retry: a
	// delivery failing right now has to be visible to an admin right now, not
	// five seconds from now.
	s.recordAttempt(id, status, err)
	if err != nil || status == http.StatusTooManyRequests || status >= 500 {
		delay := s.effectiveWebhookRetryDelay()
		if delay > 0 {
			select {
			case <-s.stopCleanup:
				// Shutting down: no second attempt. The first one's outcome is
				// what stays recorded.
				return
			case <-time.After(delay):
				status, err = s.postOnce(id, target, secret, payload)
				s.recordAttempt(id, status, err)
			}
		}
	}
}

// recordAttempt writes one attempt's outcome to last_status. An HTTP status is
// worth recording even when it is the one being retried: the next line replaces
// it, and a success after a 503 reads as HTTP 200 — the final word.
func (s *Server) recordAttempt(id string, status int, err error) {
	if err != nil {
		s.recordWebhookResult(id, "failed: "+truncate(err.Error(), 120))
		return
	}
	s.recordWebhookResult(id, fmt.Sprintf("HTTP %d", status))
}

// postOnce sends one signed copy and reports its HTTP status, or the transport
// error that stopped it. It builds the request itself so a retry starts from a
// fresh body reader — reusing a spent one would send an empty payload that the
// receiver's signature check (rightly) rejects.
func (s *Server) postOnce(id, target, secret string, payload []byte) (int, error) {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest("POST", target, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "salt.md/"+Version)
	// The receiver verifies this over the RAW body with its own copy of the
	// secret. Without it, anybody who learns the URL can forge our calls.
	req.Header.Set("X-Salt-Signature", "sha256="+sig)

	// The same guard the bulk importer uses: resolve, check EVERY address, then
	// dial the one that was checked. A webhook URL is attacker-shaped input even
	// when the attacker is a careless admin.
	//
	// webhookTransport is nil everywhere except in tests, which need to reach a
	// receiver on 127.0.0.1 — an address safeDial refuses on purpose. Injecting
	// it here rather than branching means the test still exercises this
	// function, signature and all.
	var tr http.RoundTripper = &http.Transport{DialContext: safeDial}
	if s.webhookTransport != nil {
		tr = s.webhookTransport
	}
	client := &http.Client{
		Timeout:   webhookTimeout,
		Transport: tr,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			// A redirect is a second address, and it would be checked again by
			// safeDial — but there is no reason a webhook endpoint should move,
			// and following one is how a safe URL turns into an unsafe one.
			return fmt.Errorf("a webhook endpoint must not redirect")
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func (s *Server) recordWebhookResult(id, status string) {
	s.db.Exec(`UPDATE webhooks SET last_status = ?, last_at = ? WHERE id = ?`, status, now(), id)
}
