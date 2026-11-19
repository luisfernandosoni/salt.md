package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Contextual FTS5, multilingual tiered recall and progressive disclosure.
//
// The search surface grew two kinds of noise over time. First, a page reaches
// the index with its title and its body only, so a row of a database was found
// by its own words and never by the database it belongs to or by the value of
// the field somebody actually remembers ("activo", not the whole paragraph).
// Second, an agent asking a question got twenty full snippets back — more
// context than it needed — while the same call could not say "just the outline"
// or "just this section".
//
// Both are covered here. The strict AND of the browser's search does NOT
// change: it is the promise that two words mean both words, and the relaxed
// tiers live only inside the agent's tool.

// ---- helpers ---------------------------------------------------------

func headingBlock(level int, text string) map[string]any {
	return map[string]any{
		"type":    "heading",
		"props":   map[string]any{"level": level},
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// doc stores a page with the given blocks and refreshes both indexes.
func (c *searchCase) doc(title string, blocks ...map[string]any) string {
	c.t.Helper()
	return c.page(title, blocks...)
}

// agentSearch runs the tool the way an agent does.
func agentSearch(t *testing.T, s *Server, u *user, args string) string {
	t.Helper()
	out, err := callTool(t, s, u, "search", args)
	if err != nil {
		t.Fatalf("search %s: %v", args, err)
	}
	return out
}

// ---- tiered recall ---------------------------------------------------

// Every pattern the tiers build has to be one FTS5 accepts — a pattern it
// refuses reaches the user as "no results", which is how #107 happened.
func TestProgressiveMatchVariantsAreValidFTS5(t *testing.T) {
	s := testServer(t)
	inputs := []string{
		"release", "release notes", "march release", "attached invoices",
		"draft notes", "error 123", "price display cockpit",
		"strasse map", `say "hello"`, "a OR b", "NOT this", "NEAR(x y)",
		"C++ -", "(x) y*", "col:umn", "数据库", "数据库 迁移", "API接口",
		"el informe de la cuenta", "la de los del", "der die das und",
	}
	for _, q := range inputs {
		for _, pattern := range []string{ftsMatchContentAND(q), ftsMatchRelaxedOR(q)} {
			if pattern == "" {
				continue // the tier declines rather than searches nothing
			}
			for _, table := range []string{"chunks_fts", "pages_fts"} {
				var n int
				if err := s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE `+table+` MATCH ?`, pattern).Scan(&n); err != nil {
					t.Errorf("%s: %q became %s, which FTS5 refuses: %v", table, q, pattern, err)
				}
			}
		}
	}
}

// The stopword tiers drop the words that carry no question and keep the AND
// between the rest: dropping "de" may not turn the query into an OR.
func TestContentTiersStripStopwordsAndKeepTheirJoin(t *testing.T) {
	pat := ftsMatchContentAND("el informe de la cuenta")
	if pat == "" {
		t.Fatal("the content tier declined a query with two content words")
	}
	for _, stop := range []string{`"el"`, `"la"`, `"de"`, `"der"`, `"the"`} {
		if strings.Contains(pat, stop) {
			t.Errorf("%s kept the stopword %s", pat, stop)
		}
	}
	if !strings.Contains(pat, `"informe"*`) || !strings.Contains(pat, `"cuenta"*`) {
		t.Errorf("%s lost a content word", pat)
	}
	if !strings.Contains(pat, " AND ") {
		t.Errorf("%s does not join its words with AND", pat)
	}

	or := ftsMatchRelaxedOR("informe cuenta")
	if !strings.Contains(or, " OR ") {
		t.Errorf("%s does not join its words with OR", or)
	}
	if !strings.Contains(or, `"informe"*`) || !strings.Contains(or, `"cuenta"*`) {
		t.Errorf("%s lost a content word", or)
	}

	// Nothing but stopwords: both tiers decline, so the caller keeps the
	// strict pattern it already ran instead of searching for nothing.
	if got := ftsMatchContentAND("de la el"); got != "" {
		t.Errorf("only stopwords produced %q, want the tier to decline", got)
	}
	// One content word is no OR: relaxing there would list the whole workspace.
	if got := ftsMatchRelaxedOR("informe"); got != "" {
		t.Errorf("one word produced %q, want the tier to decline", got)
	}
}

// Spanish inflection: the same stem has to reach the index's folded form, and
// only from six characters up — "mesa" must not become "mes".
func TestStemLiteTrimsSpanishEndings(t *testing.T) {
	cases := map[string]string{
		"aprobaciones": "aproba",
		"vencimientos": "venci",
		"realmente":    "real",
		"hablando":     "habl",
		"cantaron":     "cant",
		"recibieron":   "recib",
		"llegaban":     "lleg",
		"aprobadas":    "aprob",
		"concluidos":   "conclu",
		"ciudades":     "ciudad",
		"mesa":         "mesa",
		"cosas":        "cosas",
	}
	for in, want := range cases {
		if got := stemLite(in); got != want {
			t.Errorf("stemLite(%q) = %q, want %q", in, got, want)
		}
	}
	// Whatever it cuts, it keeps four characters: a stem shorter than that
	// drags half the workspace along.
	for in := range cases {
		if r := []rune(stemLite(in)); len(r) < 4 {
			t.Errorf("stemLite(%q) left %q, shorter than four runes", in, string(r))
		}
	}
}

// The tiers live in the agent's tool only. The browser's search is the place
// that promises "both words mean both words" (#107, "release elephants").
func TestTieredRecallStaysStricterInTheBrowser(t *testing.T) {
	c := newSearchCase(t)
	// "del" is on the page, "para" is not: the strict AND cannot match.
	id := c.doc("Informe mensual", paragraph("Informe del cliente sobre vencimientos. La factura va adjunta."))

	strictOnly := []string{"informe para el cliente", "factura conciliacion"}
	for _, q := range strictOnly {
		if hits := c.hits(q); len(hits) != 0 {
			t.Errorf("browser %q: got %d hits, want the strict AND to refuse them", q, len(hits))
		}
		out, err := c.s.mcpSearch(&user{ID: c.uid}, q)
		if err != nil {
			t.Fatalf("agent %q: %v", q, err)
		}
		if !strings.Contains(out, id) {
			t.Errorf("agent %q did not reach the page through the relaxed tiers:\n%s", q, out)
		}
	}

	// A query with two words that are BOTH there stays exact, and finds it.
	if hits := c.hits("informe vencimientos"); len(hits) != 1 {
		t.Errorf("strict search with both words: got %d hits, want one", len(hits))
	}
}

// ---- contextual passages ---------------------------------------------

// A row used to reach the passage index as its title and nothing else, so the
// field an agent remembers and the database it sits in were both invisible.
func TestRowPropertyAndBreadcrumbReachThePassageIndex(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "context@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)

	col := s.makeCollection(t, ws, uid, "Directorio",
		`[{"id":"estado","name":"Estado","type":"select","options":[{"id":"activo","name":"Activo"}]}]`)
	row := s.makeRow(t, ws, uid, col, "Acme", `{"estado":"Activo","rel":"`+newID()+`"}`)
	if err := s.reindexPage(col); err != nil {
		t.Fatalf("index collection: %v", err)
	}
	if err := s.reindexPage(row); err != nil {
		t.Fatalf("index row: %v", err)
	}

	// The breadcrumb (the database's title) and the property value meet in ONE
	// passage, so a two-word query over both columns still matches a row.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM chunks_fts JOIN page_chunks c ON c.id = chunks_fts.chunk_id
		WHERE chunks_fts MATCH ? AND c.page_id = ?`, ftsMatch("directorio activo"), row).Scan(&n); err != nil {
		t.Fatalf("contextual match: %v", err)
	}
	if n != 1 {
		t.Errorf("the row's passage holds no breadcrumb/property context: %d matches, want 1", n)
	}

	// The whole-page index carries the property value too.
	if err := s.db.QueryRow(`SELECT count(*) FROM pages_fts WHERE pages_fts MATCH ? AND id = ?`,
		ftsMatch("activo"), row).Scan(&n); err != nil {
		t.Fatalf("page match: %v", err)
	}
	if n != 1 {
		t.Errorf("the row's whole-page entry lost its property value")
	}

	// An agent searching the value finds the row.
	out := agentSearch(t, s, u, `{"query":"activo"}`)
	if !strings.Contains(out, row) {
		t.Errorf("agent search for a property value did not reach the row:\n%s", out)
	}

	// extractPropsText is the door: quoted keys are canonicalised, unsafe keys
	// and bare relation UUIDs are not text.
	hex := newID()
	text := extractPropsText([]byte(`{"\"estado\"":"Activo","rel":"` + hex + `","bad$key":"Nope","lista":["` + newID() + `","Visible"]}`))
	if !strings.Contains(text, "Activo") || !strings.Contains(text, "Visible") {
		t.Errorf("property values are missing from %q", text)
	}
	if strings.Contains(text, hex) {
		t.Errorf("a bare relation uuid reached the index: %q", text)
	}
	if strings.Contains(text, "Nope") {
		t.Errorf("a value under an unsafe property key reached the index: %q", text)
	}
}

// ---- get_page progressive disclosure ---------------------------------

func TestGetPageOutlineSectionAndLimits(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "outline@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	id := s.makePage(t, ws, uid, "", "Plan de cobranza", `{}`)
	content := `[` +
		`{"type":"heading","props":{"level":2},"content":[{"type":"text","text":"Resumen"}]},` +
		`{"type":"paragraph","content":[{"type":"text","text":"Texto del resumen que no debe salir en el esquema."}]},` +
		`{"type":"heading","props":{"level":2},"content":[{"type":"text","text":"Detalles"}]},` +
		`{"type":"paragraph","content":[{"type":"text","text":"Cuerpo de detalles para la seccion."}]}]`
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, content, id); err != nil {
		t.Fatal(err)
	}
	s.reindexPage(id)

	out, err := callTool(t, s, u, "get_page", `{"page_id":"`+id+`","outline_only":true}`)
	if err != nil {
		t.Fatalf("outline_only: %v", err)
	}
	for _, want := range []string{"Resumen", "Detalles", "chars"} {
		if !strings.Contains(out, want) {
			t.Errorf("outline is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Cuerpo de detalles") || strings.Contains(out, "Texto del resumen") {
		t.Errorf("outline dumped the body:\n%s", out)
	}

	// The heading match ignores case and accents.
	out, err = callTool(t, s, u, "get_page", `{"page_id":"`+id+`","section":"DETALLES"}`)
	if err != nil {
		t.Fatalf("section: %v", err)
	}
	if !strings.Contains(out, "Cuerpo de detalles") {
		t.Errorf("section did not return its own body:\n%s", out)
	}
	if strings.Contains(out, "Texto del resumen") {
		t.Errorf("section leaked the neighbouring section:\n%s", out)
	}

	out, err = callTool(t, s, u, "get_page", `{"page_id":"`+id+`","section":"No existe"}`)
	if err != nil {
		t.Fatalf("missing section: %v", err)
	}
	if !strings.Contains(out, "No section") {
		t.Errorf("a missing section dumped the page instead of saying so:\n%s", out)
	}

	// max_chars cuts between characters, never inside one.
	long := strings.Repeat("数据库迁移方案", 40)
	longID := s.makePage(t, ws, uid, "", "Largo", `{}`)
	raw, _ := json.Marshal([]map[string]any{paragraph(long)})
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, string(raw), longID); err != nil {
		t.Fatal(err)
	}
	s.reindexPage(longID)
	out, err = callTool(t, s, u, "get_page", fmt.Sprintf(`{"page_id":%q,"max_chars":80}`, longID))
	if err != nil {
		t.Fatalf("max_chars: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Error("max_chars cut inside a character")
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("max_chars says nothing about the cut:\n%s", out)
	}
	if strings.Count(out, "数据库迁移方案") >= 40 {
		t.Error("max_chars returned the whole body")
	}

	// doc_gen 0 is a real generation: a fresh page must answer "unchanged"
	// for a caller that already has 0, not fall through to the body.
	p, err := s.getPage(id)
	if err != nil {
		t.Fatal(err)
	}
	if p.DocGen != 0 {
		t.Fatalf("a page nobody rewrote from outside has doc_gen %d, want 0", p.DocGen)
	}
	out, err = callTool(t, s, u, "get_page", `{"page_id":"`+id+`","if_none_match_docgen":0}`)
	if err != nil {
		t.Fatalf("if_none_match_docgen: %v", err)
	}
	if !strings.Contains(out, "Unchanged (doc_gen: 0)") {
		t.Errorf("doc_gen 0 was treated as absent:\n%s", out)
	}
	out, err = callTool(t, s, u, "get_page", `{"page_id":"`+id+`","if_none_match_docgen":9}`)
	if err != nil {
		t.Fatalf("if_none_match_docgen stale: %v", err)
	}
	if !strings.Contains(out, "Plan de cobranza") {
		t.Errorf("a different doc_gen returned no body:\n%s", out)
	}

	// The note trail keeps its last entries when notes_limit says so.
	for _, text := range []string{"primera nota", "segunda nota", "tercera nota"} {
		if _, err := callTool(t, s, u, "note", `{"page_id":"`+id+`","text":"`+text+`"}`); err != nil {
			t.Fatalf("note: %v", err)
		}
	}
	out, err = callTool(t, s, u, "get_page", `{"page_id":"`+id+`","include_notes":true,"notes_limit":1}`)
	if err != nil {
		t.Fatalf("notes_limit: %v", err)
	}
	if !strings.Contains(out, "tercera nota") {
		t.Errorf("notes_limit dropped the newest note:\n%s", out)
	}
	if strings.Contains(out, "primera nota") || strings.Contains(out, "segunda nota") {
		t.Errorf("notes_limit kept more than the last note:\n%s", out)
	}
}

// A database answers an outline with its schema and how many rows there are —
// never with the rows themselves. That is the whole point of asking.
func TestGetPageOutlineOnCollectionCountsInsteadOfDumping(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "outline-db@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	col := s.makeCollection(t, ws, uid, "Proveedores",
		`[{"id":"estado","name":"Estado","type":"select","options":["Activo"]}]`)
	s.makeRow(t, ws, uid, col, "Acme Industrial", `{"estado":"Activo"}`)
	s.makeRow(t, ws, uid, col, "Beta Suministros", `{"estado":"Activo"}`)

	out, err := callTool(t, s, u, "get_page", `{"page_id":"`+col+`","outline_only":true}`)
	if err != nil {
		t.Fatalf("outline_only on a database: %v", err)
	}
	if !strings.Contains(out, "Estado") || !strings.Contains(out, "2") {
		t.Errorf("the schema or the row count is missing:\n%s", out)
	}
	if strings.Contains(out, "Acme Industrial") || strings.Contains(out, "Beta Suministros") {
		t.Errorf("outline_only dumped the rows:\n%s", out)
	}
}

// ---- search detail, limit, under, workspace --------------------------

func TestSearchDetailLimitUnderAndWorkspace(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "search-args@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)

	rootA := s.makePage(t, ws, uid, "", "Carpeta A", `{}`)
	childA := s.makePage(t, ws, uid, rootA, "Factura A", `{}`)
	rootB := s.makePage(t, ws, uid, "", "Carpeta B", `{}`)
	childB := s.makePage(t, ws, uid, rootB, "Factura B", `{}`)
	body, _ := json.Marshal([]map[string]any{paragraph("La factura vence el viernes y el importe es alto.")})
	for _, id := range []string{childA, childB} {
		if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, string(body), id); err != nil {
			t.Fatal(err)
		}
		s.reindexPage(id)
	}

	// compact: one line per hit, no snippet.
	out := agentSearch(t, s, u, `{"query":"factura","detail":"compact"}`)
	lines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "• ") {
			lines++
			if !strings.Contains(line, " (id: ") {
				t.Errorf("a compact line carries no id: %q", line)
			}
		} else if strings.TrimSpace(line) != "" && !strings.Contains(line, "UNTRUSTED") {
			t.Errorf("compact detail returned a second line: %q", line)
		}
	}
	if lines < 2 {
		t.Fatalf("compact detail listed %d hits, want both:\n%s", lines, out)
	}

	// passages: the passage's own text, markers stripped.
	out = agentSearch(t, s, u, `{"query":"importe","detail":"passages"}`)
	if !strings.Contains(out, "el importe es alto") {
		t.Errorf("passages did not carry the passage text:\n%s", out)
	}
	if strings.Contains(out, "\x01") || strings.Contains(out, "\x02") {
		t.Errorf("passages kept the snippet markers:\n%s", out)
	}

	// limit is respected and clamped.
	out = agentSearch(t, s, u, `{"query":"factura","limit":1}`)
	if n := strings.Count(out, "• "); n != 1 {
		t.Errorf("limit 1 returned %d hits", n)
	}

	// under: only the subtree of the page named.
	out = agentSearch(t, s, u, `{"query":"factura","under":"`+rootA+`"}`)
	if !strings.Contains(out, childA) || strings.Contains(out, childB) {
		t.Errorf("the subtree filter let a page outside it through:\n%s", out)
	}
	if _, err := callTool(t, s, u, "search", `{"query":"factura","under":"`+newID()+`"}`); err == nil {
		t.Error("under accepted a page that does not exist")
	}

	// workspace_id narrows what workspace_id may.
	other := newID()
	if _, err := s.db.Exec(`INSERT INTO workspaces (id, name, created_at, owner_id) VALUES (?, 'Otra', ?, ?)`, other, now(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES (?, ?, 'admin')`, other, uid); err != nil {
		t.Fatal(err)
	}
	elsewhere := s.makePage(t, other, uid, "", "Factura externa", `{}`)
	if _, err := s.db.Exec(`UPDATE pages SET content = ? WHERE id = ?`, string(body), elsewhere); err != nil {
		t.Fatal(err)
	}
	s.reindexPage(elsewhere)
	out = agentSearch(t, s, u, `{"query":"factura","workspace_id":"`+ws+`"}`)
	if strings.Contains(out, elsewhere) {
		t.Errorf("workspace_id let another workspace's page through:\n%s", out)
	}
	out = agentSearch(t, s, u, `{"query":"factura","workspace_id":"`+other+`"}`)
	if !strings.Contains(out, elsewhere) {
		t.Errorf("workspace_id hid the workspace it named:\n%s", out)
	}
}

// ---- query_rows fields ----------------------------------------------

func TestQueryRowsFieldsProjectThePropertyMap(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "fields@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	col := s.makeCollection(t, ws, uid, "Suscripciones",
		`[{"id":"estado","name":"Estado","type":"select","options":["Activo"]},{"id":"secreto","name":"Secreto","type":"text"}]`)
	row := s.makeRow(t, ws, uid, col, "Cliente Uno", `{"estado":"Activo","secreto":"Oculto"}`)

	out, err := callTool(t, s, u, "query_rows", `{"page_id":"`+col+`","fields":["estado"]}`)
	if err != nil {
		t.Fatalf("query_rows fields: %v", err)
	}
	if !strings.Contains(out, "Activo") || strings.Contains(out, "Oculto") {
		t.Errorf("fields did not project the properties:\n%s", out)
	}
	if !strings.Contains(out, row) || !strings.Contains(out, "Cliente Uno") {
		t.Errorf("fields dropped the row's identity:\n%s", out)
	}
}

// ---- list include_rows / max_depth ----------------------------------

func TestListOmitsRowsAndCapsDepth(t *testing.T) {
	s := testServer(t)
	uid, _ := signedIn(t, s, "list-rows@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	col := s.makeCollection(t, ws, uid, "Base de datos", `[{"id":"estado","name":"Estado","type":"text"}]`)
	s.makeRow(t, ws, uid, col, "Fila Acme", `{}`)
	s.makeRow(t, ws, uid, col, "Fila Beta", `{}`)
	root := s.makePage(t, ws, uid, "", "Manual", `{}`)
	child := s.makePage(t, ws, uid, root, "Capitulo uno", `{}`)

	out, err := callTool(t, s, u, "list", `{"kind":"pages","include_rows":false}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "[2 rows]") {
		t.Errorf("the database line says nothing about its rows:\n%s", out)
	}
	if strings.Contains(out, "Fila Acme") || strings.Contains(out, "Fila Beta") {
		t.Errorf("include_rows=false still listed the rows:\n%s", out)
	}
	if !strings.Contains(out, "Manual") {
		t.Errorf("include_rows=false dropped ordinary pages:\n%s", out)
	}

	// The default still lists them.
	out, err = callTool(t, s, u, "list", `{"kind":"pages"}`)
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if !strings.Contains(out, "Fila Acme") {
		t.Errorf("the default stopped listing rows:\n%s", out)
	}

	// max_depth 1 keeps the top level only.
	out, err = callTool(t, s, u, "list", `{"kind":"pages","max_depth":1}`)
	if err != nil {
		t.Fatalf("list max_depth: %v", err)
	}
	if strings.Contains(out, child) || strings.Contains(out, "Capitulo uno") {
		t.Errorf("max_depth 1 listed a nested page:\n%s", out)
	}
	if !strings.Contains(out, root) {
		t.Errorf("max_depth 1 dropped the top level:\n%s", out)
	}
}

// ---- safePropID hygiene ---------------------------------------------

func TestPropertyKeysAreCanonicalAndSafe(t *testing.T) {
	s := testServer(t)
	uid, cookie := signedIn(t, s, "props@example.test")
	u := &user{ID: uid, Name: "Test"}
	ws := s.firstWorkspaceOf(t, uid)
	col := s.makeCollection(t, ws, uid, "Clientes", `[{"id":"estado","name":"Estado","type":"text"}]`)
	row := s.makeRow(t, ws, uid, col, "Uno", `{"\"estado\"":"Viejo"}`)

	// A quoted key is what an agent writes when it reads the schema wrong. It
	// used to store a second, invisible property beside the real one.
	if _, err := callTool(t, s, u, "set_properties", `{"page_id":"`+row+`","properties":{"\"estado\"":"Nuevo"}}`); err != nil {
		t.Fatalf("quoted key: %v", err)
	}
	props := s.propsOf(t, row)
	if props["estado"] != "Nuevo" {
		t.Errorf("the value did not land on the real property: %#v", props)
	}
	if _, twin := props[`"estado"`]; twin {
		t.Errorf("a quoted twin survived: %#v", props)
	}

	// A key that cannot be a property id is refused, not written.
	if _, err := callTool(t, s, u, "set_properties", `{"page_id":"`+row+`","properties":{"bad$key":"x"}}`); err == nil {
		t.Error("set_properties accepted an unsafe property key")
	}

	// The browser's propsPatch takes the same road: 400 for the unsafe key,
	// and the quoted spelling heals instead of creating a twin.
	if rec := requestAs(t, s, cookie, "PATCH", "/api/pages/"+row, `{"propsPatch":{"bad$key":"x"}}`); rec.Code != 400 {
		t.Errorf("propsPatch with an unsafe key answered %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := requestAs(t, s, cookie, "PATCH", "/api/pages/"+row, `{"propsPatch":{"\"estado\"":"Parcheado"}}`); rec.Code != 200 {
		t.Fatalf("propsPatch with a quoted key answered %d: %s", rec.Code, rec.Body.String())
	}
	props = s.propsOf(t, row)
	if props["estado"] != "Parcheado" {
		t.Errorf("propsPatch did not heal the key: %#v", props)
	}
	if _, twin := props[`"estado"`]; twin {
		t.Errorf("propsPatch kept a quoted twin: %#v", props)
	}
}

// The catalogue may grow arguments, never entries: the wiki and every client
// are built against 33 tool names.
func TestProgressiveDisclosureAddsNoToolNames(t *testing.T) {
	if got := len(mcpTools); got != 33 {
		t.Errorf("the catalogue holds %d tools, want the 33 the wiki documents", got)
	}
	for _, tool := range mcpTools {
		name, _ := tool["name"].(string)
		if name != "search" && name != "get_page" && name != "query_rows" && name != "list" {
			continue
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, arg := range map[string][]string{
			"search":     {"detail", "workspace_id", "under", "limit"},
			"get_page":   {"outline_only", "section", "max_chars", "if_none_match_docgen", "notes_limit"},
			"query_rows": {"fields"},
			"list":       {"include_rows", "max_depth"},
		}[name] {
			if props[arg] == nil {
				t.Errorf("%s does not declare the optional argument %q", name, arg)
			}
		}
	}
}
