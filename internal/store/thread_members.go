package store

import (
	"database/sql"
	"strings"
)

// threadIdentity is the grouping key of one message inside an account: its
// Message-ID, or the row itself when the header is missing. With date_ts it
// matches the key the search picker already dedupes on.
func threadIdentity(alias string) string {
	return `CASE WHEN ` + alias + `.message_id = '' THEN 'id:' || ` + alias + `.id ELSE ` + alias + `.message_id END`
}

const threadVisible = `NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = m.id
	AND o.type = 'expunge' AND o.status IN ('pending', 'in_flight', 'needs_attention'))`

type threadQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// ThreadMembers returns the id of the single representative row of every
// message in the thread, in date order. A message stored in several folders
// (a Proton label row plus its All Mail row) is represented by its non-'all'
// row; a message that only has an 'all' row is represented by that row.
func (s *Store) ThreadMembers(accountID int64, threadKey string) ([]int64, error) {
	return s.ThreadMembersQ(s.DB, accountID, threadKey)
}

func (s *Store) ThreadMembersQ(q threadQuerier, accountID int64, threadKey string) ([]int64, error) {
	rows, err := q.Query(`SELECT id FROM (
		SELECT m.id AS id, m.date_ts AS date_ts,
			ROW_NUMBER() OVER (
				PARTITION BY `+threadIdentity("m")+`, m.date_ts
				ORDER BY CASE WHEN f.special_use = 'all' THEN 1 ELSE 0 END, m.id
			) AS rn
		FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id) = ?
			AND `+threadVisible+`
	) WHERE rn = 1 ORDER BY date_ts ASC, id ASC`, accountID, threadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ThreadUniqueCount counts the distinct messages of a thread in an account,
// with multi-folder rows of one message merged into one.
func (s *Store) ThreadUniqueCount(accountID int64, threadKey string) (int, error) {
	return s.ThreadUniqueCountQ(s.DB, accountID, threadKey)
}

func (s *Store) ThreadUniqueCountQ(q threadQuerier, accountID int64, threadKey string) (int, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM (
		SELECT 1 FROM messages m
		WHERE m.account_id = ? AND m.thread_key = ? AND `+threadVisible+`
		GROUP BY `+threadIdentity("m")+`, m.date_ts
	)`, accountID, threadKey).Scan(&n)
	return n, err
}

type ThreadGroupCount struct {
	Count  int
	Unread int
}

// ThreadGroupCounts counts, per thread key, the distinct messages of the
// whole group in the account and how many of them are unread. Membership is
// account-wide: members in any folder count, multi-folder rows merge.
func (s *Store) ThreadGroupCounts(accountID int64, threadKeys []string) (map[string]ThreadGroupCount, error) {
	return s.ThreadGroupCountsQ(s.DB, accountID, threadKeys)
}

func (s *Store) ThreadGroupCountsQ(q threadQuerier, accountID int64, threadKeys []string) (map[string]ThreadGroupCount, error) {
	out := make(map[string]ThreadGroupCount, len(threadKeys))
	if len(threadKeys) == 0 {
		return out, nil
	}
	ph := make([]string, len(threadKeys))
	args := make([]any, 0, len(threadKeys)+1)
	args = append(args, accountID)
	for i, k := range threadKeys {
		ph[i] = "?"
		args = append(args, k)
	}
	rows, err := q.Query(`SELECT thread_key, COUNT(*),
			COALESCE(SUM(CASE WHEN (flags & 1) = 0 THEN 1 ELSE 0 END), 0)
		FROM (
			SELECT m.thread_key AS thread_key, m.flags AS flags,
				ROW_NUMBER() OVER (
					PARTITION BY m.thread_key, `+threadIdentity("m")+`, m.date_ts
					ORDER BY CASE WHEN f.special_use = 'all' THEN 1 ELSE 0 END, m.id
				) AS rn
			FROM messages m JOIN folders f ON f.id = m.folder_id
			WHERE m.account_id = ? AND m.thread_key IN (`+strings.Join(ph, ",")+`)
				AND `+threadVisible+`
		) WHERE rn = 1 GROUP BY thread_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var gc ThreadGroupCount
		if err := rows.Scan(&key, &gc.Count, &gc.Unread); err != nil {
			return nil, err
		}
		out[key] = gc
	}
	return out, rows.Err()
}
