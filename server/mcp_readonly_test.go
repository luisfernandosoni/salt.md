package server

import (
	"strings"
	"testing"
)

// A read-only token may call the read tools and nothing else. The writing
// tools used to be listed by hand, save_as_template was missing from the list,
// and a read-only token created templates with it (reported 2026-10-23). This
// walks the whole catalogue, so a tool added later is covered the day it is.
func TestReadOnlyTokenWritesNothing(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "reader@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Plan", `{}`)
	reader := &user{ID: uid, Name: "Reader", TokenScope: "read", TokenKind: tokenKindAPI}
	refused := func(err error) bool { return err != nil && strings.Contains(err.Error(), "read-only") }
	count := func(q string) (n int) {
		s.db.QueryRow(q).Scan(&n)
		return n
	}
	pagesBefore := count(`SELECT COUNT(*) FROM pages`)

	for _, tool := range mcpTools {
		name, _ := tool["name"].(string)
		args := `{"page_id":"` + page + `"}`
		switch name {
		case "revisions":
			args = `{"page_id":"` + page + `","action":"restore","revision_id":"x"}`
		case "comments":
			args = `{"page_id":"` + page + `","action":"add","body":"x"}`
		}
		_, err := callTool(t, s, reader, name, args)
		if readTools[name] && refused(err) {
			t.Errorf("%s reads, and a read-only token was refused: %v", name, err)
		}
		if !readTools[name] && !refused(err) {
			t.Errorf("%s writes, and a read-only token got through: %v", name, err)
		}
	}
	for name := range readTools {
		if !isMCPTool(name) {
			t.Errorf("readTools names %s, which is no tool", name)
		}
	}
	// The read actions of the two mixed tools stay open to it.
	for _, name := range []string{"revisions", "comments"} {
		if _, err := callTool(t, s, reader, name, `{"page_id":"`+page+`"}`); refused(err) {
			t.Errorf("%s without a writing action was refused: %v", name, err)
		}
	}
	if n := count(`SELECT COUNT(*) FROM pages`); n != pagesBefore {
		t.Errorf("a read-only token left %d page(s) behind", n-pagesBefore)
	}
	// A name that is no tool says so, whatever the token.
	if _, err := callTool(t, s, reader, "frobnicate", `{}`); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("an unknown tool answered %v, want unknown tool", err)
	}
}
