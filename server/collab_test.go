package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A room used to compact yjs_updates on row count alone (compactThreshold, 200).
// Two or three wide frames carry more than any 200 rows of keystrokes, so a page
// that took a large paste kept its whole history in yjs_updates, and every
// WebSocket open replayed it. This room takes three frames over 64 KiB and must
// be asked to compact right there.
func TestCollabByteCompaction(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "owner@example.test")
	ws := s.firstWorkspaceOf(t, uid)
	page := s.makePage(t, ws, uid, "", "Wide page", `{}`)
	srv := httptest.NewServer(s)
	defer srv.Close()

	conn := dialCollab(t, srv, cookie, page)
	defer conn.CloseNow()
	if _, err := readUntilSynced(conn); err != nil {
		t.Fatalf("join: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 3 x 24 KiB is 72 KiB, well past compactBytesThreshold and far below
	// compactThreshold rows.
	wide := make([]byte, 24<<10)
	for i := 1; i <= 3; i++ {
		if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{frameUpdate}, wide...)); err != nil {
			t.Fatalf("write update %d: %v", i, err)
		}
	}

	upTo := readSnapshotRequest(t, ctx, conn)
	if upTo != 3 {
		t.Fatalf("snapshotRequest = %d, want 3", upTo)
	}

	snap := make([]byte, 9, 9+len("snapshot"))
	snap[0] = frameSnapshot
	binary.BigEndian.PutUint64(snap[1:9], uint64(upTo))
	snap = append(snap, []byte("snapshot")...)
	if err := conn.Write(ctx, websocket.MessageBinary, snap); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	// The snapshot is applied in the server's read loop, after the write
	// returned, so wait for it to land.
	var rows, seq int64
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, seq = 0, 0
		s.db.QueryRow(`SELECT COUNT(*) FROM yjs_updates WHERE page_id = ?`, page).Scan(&rows)
		s.db.QueryRow(`SELECT seq FROM yjs_state WHERE page_id = ?`, page).Scan(&seq)
		if rows == 0 && seq == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the snapshot: yjs_updates rows = %d, yjs_state seq = %d; want 0 and 3", rows, seq)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readSnapshotRequest reads frames until the room asks for a snapshot and
// returns the seq it asked for.
func readSnapshotRequest(t *testing.T, ctx context.Context, conn *websocket.Conn) int64 {
	t.Helper()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("reading the snapshot request: %v", err)
		}
		if typ != websocket.MessageText || !strings.Contains(string(data), `"snapshotRequest"`) {
			continue
		}
		var req struct {
			SnapshotRequest int64 `json:"snapshotRequest"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			t.Fatalf("snapshot request %q: %v", data, err)
		}
		return req.SnapshotRequest
	}
}
