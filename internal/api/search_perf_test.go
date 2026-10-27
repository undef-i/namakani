package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

type searchCorpusFixture struct {
	srv *Server
	st  *store.Store
}

func newSearchCorpusFixture(t testing.TB) *searchCorpusFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0),
		      (2, 'b', 'b@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'b@x.test', '', 0)`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive'), (3, 1, 'Trash', 'trash'), (4, 1, 'Drafts', 'drafts'),
		(5, 1, 'All Mail', 'all'), (6, 2, 'INBOX', 'inbox'), (7, 2, 'All Mail', 'all')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	return &searchCorpusFixture{srv: New(st, nil, syncmgr.NewManager(st)), st: st}
}

var searchVocab = []string{
	"report", "meeting", "budget", "travel", "family", "dinner", "project",
	"update", "invoice", "payment", "schedule", "review", "garden", "market",
	"school", "summer", "winter", "ticket", "hotel", "flight", "weekly",
	"digest", "account", "service", "notice", "confirm", "delivery",
}

func seedSearchCorpus(t testing.TB, f *searchCorpusFixture, logical int) (rows int) {
	t.Helper()
	rng := rand.New(rand.NewSource(42))
	tx, err := f.st.DB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	msgStmt, err := tx.Prepare(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, thread_key, subject, from_json, to_json, cc_json, date_ts, flags, snippet, has_attach, size)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare messages: %v", err)
	}
	defer msgStmt.Close()
	ftsStmt, err := tx.Prepare(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare fts: %v", err)
	}
	defer ftsStmt.Close()

	type copyPair struct{ inboxID, allID, account int64 }
	var dupPairs []copyPair
	var threadMembers []int64
	nextID := int64(100000)
	xylo := map[int]bool{100: true, 5000: true, 9000: true, 12000: true, 15500: true}
	for i := 0; i < logical; i++ {
		account := int64(1)
		inboxFolder, allFolder := int64(1), int64(5)
		if i >= logical*70/100 {
			account = 2
			inboxFolder, allFolder = 6, 7
		}
		ts := int64(1700000000) + rng.Int63n(280*86400)
		msgID := fmt.Sprintf("<c%d@x>", i)
		if i%50 == 49 {
			msgID = ""
		}
		threadKey := ""
		if i%10 < 3 || i%25 == 7 {
			threadKey = fmt.Sprintf("tk-%d", i/10)
		}
		subject := fmt.Sprintf("%s %s %d", searchVocab[i%len(searchVocab)], searchVocab[(i*7)%len(searchVocab)], i)
		if i >= 200 && i < 240 {
			subject = fmt.Sprintf("发票 报销 %d", i)
		}
		if i%10 == 4 {
			subject += " invoice"
		}
		words := make([]string, 0, 28)
		if i%20 < 13 {
			words = append(words, "the")
		}
		for w := 0; w < 24; w++ {
			words = append(words, searchVocab[rng.Intn(len(searchVocab))])
		}
		if i%10 == 4 {
			words = append(words, "invoice")
		}
		if xylo[i] {
			words = append(words, "xylophone")
		}
		body := strings.Join(words, " ")
		fromJSON := fmt.Sprintf(`[{"name":"Sender %d","email":"s%d@x.test"}]`, i%50, i%50)
		insert := func(folderID int64) int64 {
			id := nextID
			nextID++
			if _, err := msgStmt.Exec(id, account, folderID, 1000+id, msgID, threadKey, subject, fromJSON, `[]`, `[]`, ts, 0, body[:min(40, len(body))], 0, 1500); err != nil {
				t.Fatalf("insert message: %v", err)
			}
			if _, err := ftsStmt.Exec(id, subject, fromJSON, `[] `, body); err != nil {
				t.Fatalf("insert fts: %v", err)
			}
			rows++
			return id
		}
		inboxID := insert(inboxFolder)
		if threadKey != "" {
			threadMembers = append(threadMembers, inboxID)
		}
		if rng.Float64() < 0.30 {
			allID := insert(allFolder)
			dupPairs = append(dupPairs, copyPair{inboxID: inboxID, allID: allID, account: account})
		}
	}
	opStmt, err := tx.Prepare(`INSERT INTO ops(account_id, type, message_id, status, created_at) VALUES(?, 'expunge', ?, 'pending', 0)`)
	if err != nil {
		t.Fatalf("prepare ops: %v", err)
	}
	defer opStmt.Close()
	for i, p := range dupPairs {
		if i < 25 {
			if _, err := opStmt.Exec(p.account, p.inboxID); err != nil {
				t.Fatalf("insert op: %v", err)
			}
		} else if i < 37 {
			if _, err := opStmt.Exec(p.account, p.inboxID); err != nil {
				t.Fatalf("insert op: %v", err)
			}
			if _, err := opStmt.Exec(p.account, p.allID); err != nil {
				t.Fatalf("insert op: %v", err)
			}
		}
	}
	for i, id := range threadMembers {
		if i%211 == 0 {
			if _, err := opStmt.Exec(1, id); err != nil {
				t.Fatalf("insert thread op: %v", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rows
}

const refPickedWindow = `SELECT mm.id, ROW_NUMBER() OVER (PARTITION BY mm.account_id,
		CASE WHEN mm.message_id = '' THEN 'id:' || mm.id ELSE mm.message_id END, mm.date_ts
		ORDER BY CASE WHEN ff.special_use = 'all' THEN 1 ELSE 0 END, mm.id) AS rn FROM `
const refPickedTail = ` AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = mm.id AND o.type = 'expunge' AND o.status IN ('pending', 'in_flight', 'needs_attention'))) picked
		JOIN messages m ON m.id = picked.id
		JOIN accounts a ON a.id = m.account_id
		JOIN folders f ON f.id = m.folder_id
		WHERE picked.rn = 1 ORDER BY m.date_ts DESC, m.id DESC LIMIT ? OFFSET ?`
const refSearchCols = `SELECT m.id, m.account_id, a.label, m.folder_id, f.name, f.special_use, m.thread_key, m.subject,
		m.from_json, m.date_ts, m.flags, m.has_attach, m.snippet, m.size,
		(SELECT COUNT(*) FROM (SELECT 1 FROM messages t WHERE t.account_id = m.account_id AND t.thread_key = m.thread_key AND m.thread_key <> '' AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = t.id AND o.type = 'expunge' AND o.status IN ('pending', 'in_flight', 'needs_attention')) GROUP BY CASE WHEN t.message_id = '' THEN 'id:' || t.id ELSE t.message_id END, t.date_ts))
		FROM (`

type searchRefRow struct {
	ID    int64
	Count int
}

func searchIsShort(q string) (terms []string, short bool) {
	terms = strings.Fields(q)
	short = len(terms) > 0
	for _, t := range terms {
		if len([]rune(t)) >= 3 {
			short = false
		}
	}
	return terms, short
}

func referenceSQL(q string, limit, offset int) (string, []any) {
	terms, short := searchIsShort(q)
	if short {
		conds := make([]string, 0, len(terms))
		args := make([]any, 0, len(terms)*4+2)
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
		for _, t := range terms {
			like := "%" + esc.Replace(t) + "%"
			conds = append(conds, `(mm.subject LIKE ? ESCAPE '\' OR mm.from_json LIKE ? ESCAPE '\' OR mm.to_json LIKE ? ESCAPE '\' OR mm.cc_json LIKE ? ESCAPE '\')`)
			args = append(args, like, like, like, like)
		}
		args = append(args, limit, offset)
		return refSearchCols + refPickedWindow + `messages mm
			JOIN folders ff ON ff.id = mm.folder_id
			WHERE ` + strings.Join(conds, " AND ") + refPickedTail, args
	}
	return refSearchCols + refPickedWindow + `messages_fts
			JOIN messages mm ON mm.id = messages_fts.rowid
			JOIN folders ff ON ff.id = mm.folder_id
			WHERE messages_fts MATCH ?` + refPickedTail,
		[]any{store.BuildFTSQuery(q), limit, offset}
}

func referenceSearch(db *sql.DB, q string, limit, offset int) ([]searchRefRow, error) {
	query, args := referenceSQL(q, limit, offset)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []searchRefRow{}
	for rows.Next() {
		var id, accountID, folderID, dateTS, flags, size int64
		var label, folderName, folderSpecial, threadKey, subject, fromJSON, snippet string
		var hasAttach, cnt int
		if err := rows.Scan(&id, &accountID, &label, &folderID, &folderName, &folderSpecial, &threadKey,
			&subject, &fromJSON, &dateTS, &flags, &hasAttach, &snippet, &size, &cnt); err != nil {
			return nil, err
		}
		out = append(out, searchRefRow{ID: id, Count: cnt})
	}
	return out, rows.Err()
}

type searchPageRow struct {
	ID        int64
	AccountID int64
	ThreadKey string
}

func streamSearchPage(db *sql.DB, q string, limit, offset int) ([]searchPageRow, error) {
	special := map[int64]string{}
	frows, err := db.Query("SELECT id, special_use FROM folders")
	if err != nil {
		return nil, err
	}
	for frows.Next() {
		var id int64
		var su string
		if err := frows.Scan(&id, &su); err != nil {
			frows.Close()
			return nil, err
		}
		special[id] = su
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return nil, err
	}
	expunging := map[int64]bool{}
	orows, err := db.Query("SELECT message_id FROM ops WHERE type = 'expunge' AND status IN ('pending', 'in_flight', 'needs_attention')")
	if err != nil {
		return nil, err
	}
	for orows.Next() {
		var id int64
		if err := orows.Scan(&id); err != nil {
			orows.Close()
			return nil, err
		}
		expunging[id] = true
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		return nil, err
	}

	terms, short := searchIsShort(q)
	type scanRow struct {
		id, accountID, dateTS, folderID int64
		messageID, threadKey            string
	}
	type groupKey struct {
		account int64
		key     string
	}
	type candidate struct {
		id        int64
		account   int64
		pref      int
		threadKey string
	}
	out := []searchPageRow{}
	skipped := 0
	var bucketDate int64 = -1
	bucket := map[groupKey]candidate{}
	flush := func() bool {
		reps := make([]candidate, 0, len(bucket))
		for _, c := range bucket {
			reps = append(reps, c)
		}
		sort.Slice(reps, func(i, j int) bool { return reps[i].id > reps[j].id })
		for _, c := range reps {
			if skipped < offset {
				skipped++
				continue
			}
			out = append(out, searchPageRow{ID: c.id, AccountID: c.account, ThreadKey: c.threadKey})
			if len(out) >= limit {
				return true
			}
		}
		bucket = map[groupKey]candidate{}
		return false
	}
	consume := func(r scanRow) bool {
		if expunging[r.id] {
			return false
		}
		if bucketDate == -1 {
			bucketDate = r.dateTS
		}
		if r.dateTS != bucketDate {
			if flush() {
				return true
			}
			bucketDate = r.dateTS
		}
		key := r.messageID
		if key == "" {
			key = fmt.Sprintf("id:%d", r.id)
		}
		gk := groupKey{account: r.accountID, key: key}
		pref := 0
		if special[r.folderID] == "all" {
			pref = 1
		}
		cur, ok := bucket[gk]
		if !ok || pref < cur.pref || (pref == cur.pref && r.id < cur.id) {
			bucket[gk] = candidate{id: r.id, account: r.accountID, pref: pref, threadKey: r.threadKey}
		}
		return false
	}

	if short {
		conds := make([]string, 0, len(terms))
		args := make([]any, 0, len(terms)*4)
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
		for _, t := range terms {
			like := "%" + esc.Replace(t) + "%"
			conds = append(conds, `(m.subject LIKE ? ESCAPE '\' OR m.from_json LIKE ? ESCAPE '\' OR m.to_json LIKE ? ESCAPE '\' OR m.cc_json LIKE ? ESCAPE '\')`)
			args = append(args, like, like, like, like)
		}
		rows, err := db.Query(`SELECT m.id, m.account_id, m.message_id, m.date_ts, m.folder_id, m.thread_key FROM messages m WHERE `+
			strings.Join(conds, " AND "), args...)
		if err != nil {
			return nil, err
		}
		var found []scanRow
		for rows.Next() {
			var r scanRow
			if err := rows.Scan(&r.id, &r.accountID, &r.messageID, &r.dateTS, &r.folderID, &r.threadKey); err != nil {
				rows.Close()
				return nil, err
			}
			found = append(found, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		sort.Slice(found, func(i, j int) bool {
			if found[i].dateTS != found[j].dateTS {
				return found[i].dateTS > found[j].dateTS
			}
			return found[i].id > found[j].id
		})
		for _, r := range found {
			if consume(r) {
				return out, nil
			}
		}
		flush()
		return out, nil
	}

	query, args := streamSQL(q)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r scanRow
		if err := rows.Scan(&r.id, &r.accountID, &r.messageID, &r.dateTS, &r.folderID, &r.threadKey); err != nil {
			return nil, err
		}
		if consume(r) {
			return out, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

func streamSQL(q string) (string, []any) {
	return `SELECT m.id, m.account_id, m.message_id, m.date_ts, m.folder_id, m.thread_key FROM messages m INDEXED BY idx_messages_date WHERE m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?) ORDER BY m.date_ts DESC, m.id DESC`,
		[]any{store.BuildFTSQuery(q)}
}

func streamSearchCounts(db *sql.DB, page []searchPageRow) (map[int64]int, error) {
	counts := map[int64]int{}
	type pair struct {
		account int64
		key     string
	}
	seen := map[pair]bool{}
	var pairs []pair
	for _, r := range page {
		counts[r.ID] = 0
		if r.ThreadKey == "" {
			continue
		}
		p := pair{r.AccountID, r.ThreadKey}
		if !seen[p] {
			seen[p] = true
			pairs = append(pairs, p)
		}
	}
	if len(pairs) == 0 {
		return counts, nil
	}
	conds := make([]string, 0, len(pairs))
	args := make([]any, 0, len(pairs)*2)
	for _, p := range pairs {
		conds = append(conds, "(t.account_id = ? AND t.thread_key = ?)")
		args = append(args, p.account, p.key)
	}
	rows, err := db.Query(`SELECT g.account_id, g.thread_key, COUNT(*) FROM (
		SELECT t.account_id AS account_id, t.thread_key AS thread_key FROM messages t
		WHERE (`+strings.Join(conds, " OR ")+`)
		AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = t.id AND o.type = 'expunge' AND o.status IN ('pending', 'in_flight', 'needs_attention'))
		GROUP BY t.account_id, t.thread_key, CASE WHEN t.message_id = '' THEN 'id:' || t.id ELSE t.message_id END, t.date_ts
	) g GROUP BY g.account_id, g.thread_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byPair := map[pair]int{}
	for rows.Next() {
		var account int64
		var key string
		var n int
		if err := rows.Scan(&account, &key, &n); err != nil {
			return nil, err
		}
		byPair[pair{account, key}] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range page {
		if r.ThreadKey != "" {
			counts[r.ID] = byPair[pair{r.AccountID, r.ThreadKey}]
		}
	}
	return counts, nil
}

func explainPlan(db *sql.DB, query string, args ...any) (string, error) {
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return "", err
		}
		b.WriteString(detail)
		b.WriteByte('\n')
	}
	return b.String(), rows.Err()
}

func (f *searchCorpusFixture) searchViaHandler(t testing.TB, q string, limit, offset int) []searchRefRow {
	t.Helper()
	v := url.Values{"q": {q}, "limit": {fmt.Sprint(limit)}, "offset": {fmt.Sprint(offset)}}
	req := httptest.NewRequest(http.MethodGet, "/api/search?"+v.Encode(), nil)
	rec := httptest.NewRecorder()
	f.srv.handleSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler search %q: status %d body %s", q, rec.Code, rec.Body.String())
	}
	var out []threadOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("handler search %q: decode: %v", q, err)
	}
	rows := []searchRefRow{}
	for _, r := range out {
		rows = append(rows, searchRefRow{ID: r.ID, Count: r.ThreadCount})
	}
	return rows
}

func streamSearchFull(db *sql.DB, q string, limit, offset int) ([]searchRefRow, error) {
	page, err := streamSearchPage(db, q, limit, offset)
	if err != nil {
		return nil, err
	}
	counts, err := streamSearchCounts(db, page)
	if err != nil {
		return nil, err
	}
	out := []searchRefRow{}
	for _, r := range page {
		out = append(out, searchRefRow{ID: r.ID, Count: counts[r.ID]})
	}
	return out, nil
}

func requireSameRows(t testing.TB, what string, want, got []searchRefRow) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: %d rows, want %d\nwant %v\ngot  %v", what, len(got), len(want), want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("%s: row %d = %+v, want %+v\nwant %v\ngot  %v", what, i, got[i], want[i], want, got)
		}
	}
}

func TestSearchStreamMatchesReferenceSmall(t *testing.T) {
	f := newSearchCorpusFixture(t)
	seedSearchCorpus(t, f, 140)
	extra := []struct {
		id, folder int64
		msgID      string
		ts         int64
		subject    string
	}{
		{900001, 1, "<dup@x>", 1700500000, "the duplex case one"},
		{900002, 2, "<dup@x>", 1700600000, "the duplex case two"},
		{900003, 1, "", 1700550000, "the emptyid case"},
		{900004, 5, "", 1700550000, "the emptyid case"},
		{900005, 1, "<fp@x>", 1700570000, "发票 报销 手册"},
		{900006, 1, "<ai@x>", 1700580000, "AI weekly notes"},
	}
	for _, e := range extra {
		if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, subject, from_json, date_ts)
			VALUES(?, 1, ?, ?, ?, ?, '[{"name":"Edge","email":"edge@x.test"}]', ?)`, e.id, e.folder, 5000+e.id, e.msgID, e.subject, e.ts); err != nil {
			t.Fatalf("insert edge %d: %v", e.id, err)
		}
		if _, err := f.st.DB.Exec(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body)
			VALUES(?, ?, 'edge@x.test', '', 'the edge body')`, e.id, e.subject); err != nil {
			t.Fatalf("insert edge fts %d: %v", e.id, err)
		}
	}
	queries := []string{"the", "invoice", "xylophone", "发票", "报销", "AI", "weekly digest", "the invoice", "duplex", "emptyid", "edge", "nonexistentword"}
	for _, q := range queries {
		for _, limit := range []int{1, 2, 3, 7, 60} {
			for offset := 0; offset <= 42; offset += 3 {
				want, err := referenceSearch(f.st.DB, q, limit, offset)
				if err != nil {
					t.Fatalf("reference %q: %v", q, err)
				}
				requireSameRows(t, fmt.Sprintf("handler q=%q limit=%d offset=%d", q, limit, offset),
					want, f.searchViaHandler(t, q, limit, offset))
				gotStream, err := streamSearchFull(f.st.DB, q, limit, offset)
				if err != nil {
					t.Fatalf("stream %q: %v", q, err)
				}
				requireSameRows(t, fmt.Sprintf("stream q=%q limit=%d offset=%d", q, limit, offset), want, gotStream)
			}
		}
	}
}

func TestSearchStreamMatchesReferenceAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale corpus skipped in -short mode")
	}
	f := newSearchCorpusFixture(t)
	start := time.Now()
	rows := seedSearchCorpus(t, f, 12300)
	t.Logf("seeded %d message rows in %v", rows, time.Since(start))

	oldQ, oldArgs := referenceSQL("the", 60, 0)
	plan, err := explainPlan(f.st.DB, oldQ, oldArgs...)
	if err != nil {
		t.Fatalf("explain old: %v", err)
	}
	t.Logf("OLD plan (q=the):\n%s", plan)
	newQ, newArgs := streamSQL("the")
	plan, err = explainPlan(f.st.DB, newQ, newArgs...)
	if err != nil {
		t.Fatalf("explain new: %v", err)
	}
	t.Logf("NEW plan (q=the):\n%s", plan)

	type pageSpec struct{ limit, offset int }
	pages := []pageSpec{{60, 0}, {60, 60}, {60, 120}, {60, 300}, {60, 600}, {7, 0}, {7, 7}, {7, 70}, {200, 0}}
	for _, q := range []string{"the", "invoice", "xylophone", "发票", "the invoice"} {
		var refTotal, streamTotal time.Duration
		for _, p := range pages {
			s := time.Now()
			want, err := referenceSearch(f.st.DB, q, p.limit, p.offset)
			if err != nil {
				t.Fatalf("reference %q: %v", q, err)
			}
			refTotal += time.Since(s)
			s = time.Now()
			got, err := streamSearchFull(f.st.DB, q, p.limit, p.offset)
			if err != nil {
				t.Fatalf("stream %q: %v", q, err)
			}
			streamTotal += time.Since(s)
			requireSameRows(t, fmt.Sprintf("scale q=%q limit=%d offset=%d", q, p.limit, p.offset), want, got)
		}
		t.Logf("q=%q over %d pages: reference %v, stream %v", q, len(pages), refTotal, streamTotal)
	}
	for _, q := range []string{"the", "invoice", "xylophone", "发票", "the invoice"} {
		for _, p := range []pageSpec{{60, 0}, {60, 600}, {7, 70}, {200, 0}} {
			want, err := referenceSearch(f.st.DB, q, p.limit, p.offset)
			if err != nil {
				t.Fatalf("reference %q: %v", q, err)
			}
			requireSameRows(t, fmt.Sprintf("scale handler q=%q limit=%d offset=%d", q, p.limit, p.offset),
				want, f.searchViaHandler(t, q, p.limit, p.offset))
		}
	}
}

func benchSearchFixture(b *testing.B) *searchCorpusFixture {
	b.Helper()
	f := newSearchCorpusFixture(b)
	seedSearchCorpus(b, f, 12300)
	return f
}

func BenchmarkSearchHandlerHighFreq(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.searchViaHandler(b, "the", 60, 0)
	}
}

func BenchmarkSearchHandlerHighFreqDeep(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.searchViaHandler(b, "the", 60, 600)
	}
}

func BenchmarkSearchHandlerMidFreq(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.searchViaHandler(b, "invoice", 60, 0)
	}
}

func BenchmarkSearchHandlerLowFreq(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.searchViaHandler(b, "xylophone", 60, 0)
	}
}

func BenchmarkSearchHandlerShortTerm(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.searchViaHandler(b, "发票", 60, 0)
	}
}

func BenchmarkSearchReferenceHighFreq(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := referenceSearch(f.st.DB, "the", 60, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchReferenceHighFreqDeep(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := referenceSearch(f.st.DB, "the", 60, 600); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchReferenceMidFreq(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := referenceSearch(f.st.DB, "invoice", 60, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchReferenceLowFreq(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := referenceSearch(f.st.DB, "xylophone", 60, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchReferenceShortTerm(b *testing.B) {
	f := benchSearchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := referenceSearch(f.st.DB, "发票", 60, 0); err != nil {
			b.Fatal(err)
		}
	}
}
