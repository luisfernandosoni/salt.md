package server

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// authenticateToken wrote api_tokens.last_used_at and last_used_ip on every
// bearer request, through the single write connection. An agent makes hundreds
// of calls a minute and every one of them queued behind the writer for a row
// nobody reads at that resolution.
func TestAPITokenTouchDebounceAndIPChange(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "owner@example.test")
	token := "a-key-for-the-touch-test"
	const tokID = "tok_touch"
	if _, err := s.db.Exec(`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, created_at)
		VALUES (?, ?, 'agent', ?, 'write', ?)`, tokID, uid, tokenHash(token), now()); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	bearer := map[string]string{"Authorization": "Bearer " + token}

	// The token must be accepted, or none of this measures anything.
	if rec := call(s, "GET", "/api/pages", "203.0.113.9", bearer, ""); rec.Code != 200 {
		t.Fatalf("bearer call: %d %s", rec.Code, rec.Body.String())
	}
	firstAt, firstIP := tokenTouch(t, s, tokID)
	if firstIP != "203.0.113.9" {
		t.Fatalf("first touch recorded ip %q", firstIP)
	}
	if firstAt == "" {
		t.Fatal("first touch recorded no timestamp")
	}

	// A second call from the same address inside the window must not rewrite
	// the row. Sleep past a millisecond so a write would be visible.
	time.Sleep(5 * time.Millisecond)
	if rec := call(s, "GET", "/api/pages", "203.0.113.9", bearer, ""); rec.Code != 200 {
		t.Fatalf("second bearer call: %d %s", rec.Code, rec.Body.String())
	}
	secondAt, secondIP := tokenTouch(t, s, tokID)
	if secondAt != firstAt || secondIP != firstIP {
		t.Fatalf("same address within a minute rewrote the row: %q/%q then %q/%q", firstAt, firstIP, secondAt, secondIP)
	}

	// A different address is a fact worth recording at once: that is how a
	// token used from somewhere new becomes visible.
	if rec := call(s, "GET", "/api/pages", "198.51.100.7", bearer, ""); rec.Code != 200 {
		t.Fatalf("third bearer call: %d %s", rec.Code, rec.Body.String())
	}
	_, thirdIP := tokenTouch(t, s, tokID)
	if thirdIP != "198.51.100.7" {
		t.Fatalf("a new address did not update last_used_ip: %q", thirdIP)
	}

	// The reader pool must take concurrent readers. On a file-backed database
	// the write connection is one, so a read pool that does not exist leaves
	// every one of these queued behind the writer.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var n int
			if err := s.readDB().QueryRow(`SELECT COUNT(*) FROM pages`).Scan(&n); err != nil {
				errs <- fmt.Errorf("reader %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func tokenTouch(t *testing.T, s *Server, id string) (at, ip string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT last_used_at, last_used_ip FROM api_tokens WHERE id = ?`, id).Scan(&at, &ip); err != nil {
		t.Fatalf("reading the token: %v", err)
	}
	return at, ip
}
