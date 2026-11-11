package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Document generations (#79).
//
// A page's body lives twice: as the CRDT document the editors share
// (yjs_state + yjs_updates, relayed by collab.go and never interpreted here),
// and as its materialized copy in pages.content, which an editor writes back
// a moment after every change (PATCH ?materialize=1) so that search, export
// and the API read current text.
//
// A write from OUTSIDE the editor (REST content, MCP write_content, restore,
// import, upload_file) replaces pages.content and then throws the CRDT doc
// away (resetYjsDoc): open editors are closed with 4001 and re-seed from the
// new text. Three doors let an editor's copy of the OLD document back in over
// the new one, all silently:
//
//  1. Its autosave. The debounced write, or the flush when the editor
//     unmounts, which the 4001 itself causes. The server took any
//     materialize write, so typing in the second before an agent's rewrite
//     was enough to put the old text back, and the rewrite was in no revision.
//  2. Its reconnect. A tab whose socket was down at the moment of the reset
//     (a laptop asleep, Wi-Fi, a proxy, a deploy) never hears the 4001. On
//     reconnect it pushes its whole local state, which becomes the live
//     document, or is merged into the re-seeded one, where the editor then
//     drops one of the two bodies.
//  3. Its seed. An editor loads the body (GET) and then joins the live
//     document; a rewrite landing in between left it seeding the new live
//     document from the old body. An agent writing twice in a row is enough:
//     the first write sends the open editor to load again, the second lands
//     while it does.
//
// pages.doc_gen numbers the documents. Every outside write moves it in the
// SAME statement that replaces the content (bumpDocGen), so there is no moment
// in which the new text is stored while a copy of the old one still passes.
// The editor learns the number with the body it loads (GET /api/pages/{id}
// carries docGen) and names it on every connect and every autosave; a stale
// number is refused, with a 4001 close on the socket and 409 doc_replaced on
// the autosave. A refused autosave is not lost: it goes into the page history
// (keepRefusedCopy).

// bumpDocGen belongs in the SET list of every statement that replaces a page's
// body from outside the editor. Writes by the editor itself never carry it.
const bumpDocGen = "doc_gen = doc_gen + 1"

// docGen is the generation of a page's live document (0 for a page whose body
// has never been replaced from outside).
func (s *Server) docGen(pageID string) int64 {
	var gen int64
	s.db.QueryRow(`SELECT doc_gen FROM pages WHERE id = ?`, pageID).Scan(&gen)
	return gen
}

// materializeGuard returns the condition an editor's autosave has to meet to
// be written: it must descend from the current document. ok=false means the
// request named a generation that is not a number.
//
// A frontend from before this check names no generation. For those the
// best the server can tell is whether a live document exists at all: right
// after a reset there is none until an editor re-seeds it from the new text,
// and that is exactly when the stale flush of a just-kicked editor arrives.
// An editor that may legitimately save has always sent its updates over the
// socket first, so the document exists by then.
//
// replaced=1 is an editor that already knows (it got the 4001) and sends its
// last copy only to have it kept. That one is never written, whatever its
// generation: an editor that joined in the instant between an outside write
// and the reset holds the new number with the old text.
func materializeGuard(r *http.Request) (clause string, args []any, ok bool) {
	if r.URL.Query().Get("replaced") == "1" {
		return " AND 0", nil, true
	}
	g := r.URL.Query().Get("gen")
	if g == "" {
		return ` AND (EXISTS (SELECT 1 FROM yjs_state WHERE page_id = pages.id)
			OR EXISTS (SELECT 1 FROM yjs_updates WHERE page_id = pages.id))`, nil, true
	}
	gen, err := strconv.ParseInt(g, 10, 64)
	if err != nil {
		return "", nil, false
	}
	return " AND doc_gen = ?", []any{gen}, true
}

// docGenConflict is the refusal an outside write makes when the page has moved
// on since the caller looked at it. Same shape and same 409 marker as the
// editor's doc_replaced answer, so a caller can branch on it either way.
func docGenConflict(pageID string, actual, expected int64) error {
	return fmt.Errorf("409 Conflict: page %q doc_gen is %d (expected %d) — fetch latest page state before writing", pageID, actual, expected)
}

// keepRefusedCopy files an editor's refused copy in the page history. It is a
// copy of the replaced document plus whatever was typed into it since its
// last save, which can be a whole offline session; refusing it must not mean
// losing it. The same copy twice (two tabs on one stale document) is kept
// once.
func (s *Server) keepRefusedCopy(pageID string, u *user, content string) {
	if strings.TrimSpace(content) == "" || content == "[]" {
		return
	}
	var title, newest string
	s.db.QueryRow(`SELECT title FROM pages WHERE id = ?`, pageID).Scan(&title)
	s.db.QueryRow(`SELECT content FROM page_revisions WHERE page_id = ? ORDER BY created_at DESC LIMIT 1`, pageID).Scan(&newest)
	if newest == content {
		return
	}
	s.db.Exec(`INSERT INTO page_revisions (id, page_id, created_at, author_id, author_name, title, content) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		newID(), pageID, now(), u.ID, u.Name, title, content)
	s.db.Exec(`DELETE FROM page_revisions WHERE page_id = ? AND id NOT IN (
		SELECT id FROM page_revisions WHERE page_id = ? ORDER BY created_at DESC LIMIT ?)`, pageID, pageID, revisionKeep)
}
