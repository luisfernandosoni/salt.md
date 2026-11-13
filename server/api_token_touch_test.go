package server

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// The reader pool is built from the database path, so the file-backed server
// every other test uses must actually get one. Without this, a regression that
// silently fell back to the writer would pass every test below by reading
// through Server.db.
func TestServerOpensReadPool(t *testing.T) {
	s := testServer(t)
	if s.rdb == nil {
		t.Fatal("expected file-backed server to open s.rdb")
	}
	if s.readDB() != s.rdb {
		t.Fatal("readDB did not return the reader pool")
	}
}

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
	firstAt, firstIP := apiTokenTouch(t, s, tokID)
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
	secondAt, secondIP := apiTokenTouch(t, s, tokID)
	if secondAt != firstAt || secondIP != firstIP {
		t.Fatalf("same address within a minute rewrote the row: %q/%q then %q/%q", firstAt, firstIP, secondAt, secondIP)
	}

	// A different address is a fact worth recording at once: that is how a
	// token used from somewhere new becomes visible.
	if rec := call(s, "GET", "/api/pages", "198.51.100.7", bearer, ""); rec.Code != 200 {
		t.Fatalf("third bearer call: %d %s", rec.Code, rec.Body.String())
	}
	_, thirdIP := apiTokenTouch(t, s, tokID)
	if thirdIP != "198.51.100.7" {
		t.Fatalf("a new address did not update last_used_ip: %q", thirdIP)
	}

	// The reader pool must take concurrent readers. On a file-backed database
	// the write connection is one, so a read pool that does not exist leaves
	// every one of these queued behind the writer.
	assertConcurrentReaders(t, s)
}

// An OAuth access token used to rewrite its grant's last_used_* columns on every
// request, the same unconditional UPDATE an API token did. The grant row is the
// one the consent screen shows, so it is read at human resolution and written
// on every agent call.
func TestOAuthGrantTouchDebounceAndIPChange(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "owner@example.test")
	const grantID = "grant_touch"
	token := "an-oauth-access-token-for-the-test"
	if _, err := s.db.Exec(`INSERT INTO oauth_grants (id, user_id, client_id, refresh_hash, scope, created_at)
		VALUES (?, ?, 'client_touch', ?, 'write', ?)`, grantID, uid, tokenHash("refresh-touch"), now()); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO oauth_access (token_hash, grant_id, expires_at) VALUES (?, ?, ?)`,
		tokenHash(token), grantID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert access token: %v", err)
	}
	bearer := map[string]string{"Authorization": "Bearer " + token}

	if rec := call(s, "GET", "/api/pages", "203.0.113.11", bearer, ""); rec.Code != 200 {
		t.Fatalf("oauth bearer call: %d %s", rec.Code, rec.Body.String())
	}
	firstAt, firstIP := oauthGrantTouch(t, s, grantID)
	if firstIP != "203.0.113.11" || firstAt == "" {
		t.Fatalf("first use recorded %q/%q, want the address and a timestamp", firstAt, firstIP)
	}

	time.Sleep(5 * time.Millisecond)
	if rec := call(s, "GET", "/api/pages", "203.0.113.11", bearer, ""); rec.Code != 200 {
		t.Fatalf("second oauth bearer call: %d %s", rec.Code, rec.Body.String())
	}
	secondAt, secondIP := oauthGrantTouch(t, s, grantID)
	if secondAt != firstAt || secondIP != firstIP {
		t.Fatalf("same address within a minute rewrote the grant: %q/%q then %q/%q", firstAt, firstIP, secondAt, secondIP)
	}

	if rec := call(s, "GET", "/api/pages", "198.51.100.23", bearer, ""); rec.Code != 200 {
		t.Fatalf("third oauth bearer call: %d %s", rec.Code, rec.Body.String())
	}
	_, thirdIP := oauthGrantTouch(t, s, grantID)
	if thirdIP != "198.51.100.23" {
		t.Fatalf("a new address did not update the grant's last_used_ip: %q", thirdIP)
	}
}

// shouldTouchToken is the whole decision, so the two columns that are easy to
// get backwards get their own table.
func TestShouldTouchToken(t *testing.T) {
	recent := time.Now().UTC().Add(-2 * time.Second).Format(time.RFC3339Nano)
	old := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339Nano)
	cases := []struct {
		name         string
		at, was, now string
		want         bool
	}{
		{"first use", "", "", "203.0.113.9", true},
		{"same address, recent", recent, "203.0.113.9", "203.0.113.9", false},
		{"new address, recent", recent, "203.0.113.9", "198.51.100.7", true},
		{"same address, old", old, "203.0.113.9", "203.0.113.9", true},
		{"unparseable timestamp", "yesterday", "203.0.113.9", "203.0.113.9", true},
	}
	for _, c := range cases {
		if got := shouldTouchToken(c.at, c.was, c.now); got != c.want {
			t.Errorf("%s: shouldTouchToken(%q, %q, %q) = %v, want %v", c.name, c.at, c.was, c.now, got, c.want)
		}
	}
}

func assertConcurrentReaders(t *testing.T, s *Server) {
	t.Helper()
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

func apiTokenTouch(t *testing.T, s *Server, id string) (at, ip string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT COALESCE(last_used_at, ''), COALESCE(last_used_ip, '') FROM api_tokens WHERE id = ?`, id).Scan(&at, &ip); err != nil {
		t.Fatalf("reading the token: %v", err)
	}
	return at, ip
}

func oauthGrantTouch(t *testing.T, s *Server, id string) (at, ip string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT COALESCE(last_used_at, ''), COALESCE(last_used_ip, '') FROM oauth_grants WHERE id = ?`, id).Scan(&at, &ip); err != nil {
		t.Fatalf("reading the grant: %v", err)
	}
	return at, ip
}
