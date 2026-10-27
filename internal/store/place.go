package store

import (
	"fmt"
	"strings"
)

func (s *Store) ListVersion() (int64, error) {
	var v int64
	err := s.DB.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM place_log`).Scan(&v)
	return v, err
}

func (s *Store) PlacementVersions(ids []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.DB.Query(`SELECT message_id, MAX(seq) FROM place_log
		WHERE message_id IN (`+strings.Join(ph, ",")+`) GROUP BY message_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("placement versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, v int64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, fmt.Errorf("placement versions: %w", err)
		}
		out[id] = v
	}
	return out, rows.Err()
}
