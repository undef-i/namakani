package sync

import (
	"log"

	"namakani/internal/mail"
)

func (m *Manager) RecomputeSnippets() {
	rows, err := m.st.DB.Query(
		`SELECT b.message_id, b.text_body, b.html_body, msg.snippet
		   FROM bodies b JOIN messages msg ON msg.id = b.message_id`)
	if err != nil {
		log.Printf("snippet recompute: query: %v", err)
		return
	}
	type item struct {
		id              int64
		text, html, old string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.text, &it.html, &it.old); err != nil {
			break
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("snippet recompute: read: %v", err)
		return
	}
	changed := 0
	for _, it := range items {
		if ns := mail.MakeSnippet(it.text, it.html); ns != it.old {
			if _, err := m.st.DB.Exec("UPDATE messages SET snippet = ? WHERE id = ?", ns, it.id); err == nil {
				changed++
			}
		}
	}
	log.Printf("snippet recompute: %d/%d updated", changed, len(items))
}
