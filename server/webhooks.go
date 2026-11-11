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

	for _, t := range targets {
		go s.deliverWebhook(t.id, t.url, t.secret, payload)
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

func (s *Server) deliverWebhook(id, target, secret string, payload []byte) {
	defer func() {
		// A panic in a background goroutine takes the whole server with it, and
		// a webhook is the last thing that should be able to do that.
		if rec := recover(); rec != nil {
			log.Printf("webhook %s: recovered from %v", id, rec)
		}
	}()

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest("POST", target, bytes.NewReader(payload))
	if err != nil {
		s.recordWebhookResult(id, "bad request: "+err.Error())
		return
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
		s.recordWebhookResult(id, "failed: "+truncate(err.Error(), 120))
		return
	}
	defer resp.Body.Close()
	s.recordWebhookResult(id, fmt.Sprintf("HTTP %d", resp.StatusCode))
}

func (s *Server) recordWebhookResult(id, status string) {
	s.db.Exec(`UPDATE webhooks SET last_status = ?, last_at = ? WHERE id = ?`, status, now(), id)
}
