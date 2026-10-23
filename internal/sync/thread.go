package sync

import (
	"context"
	"log"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func (m *Manager) repairThreads(ctx context.Context, cl *mail.Client, acct mail.Account) {
	size := m.st.AppSettings().SyncBatch
	rows, err := m.st.ThreadRepairRows(acct.ID)
	if err != nil {
		log.Printf("thread repair account %d: candidates: %v", acct.ID, err)
		return
	}
	if len(rows) == 0 {
		return
	}
	byFolder := make(map[string][]store.ThreadRepairRow)
	var order []string
	for _, r := range rows {
		if _, ok := byFolder[r.FolderName]; !ok {
			order = append(order, r.FolderName)
		}
		byFolder[r.FolderName] = append(byFolder[r.FolderName], r)
	}
	fixed := 0
	for _, fname := range order {
		if ctx.Err() != nil {
			return
		}
		group := byFolder[fname]
		if _, err := cl.Select(fname); err != nil {
			log.Printf("thread repair account %d: select %s: %v", acct.ID, fname, err)
			continue
		}
		for start := 0; start < len(group); start += size {
			end := start + size
			if end > len(group) {
				end = len(group)
			}
			batch := group[start:end]
			uids := make([]uint32, 0, len(batch))
			for _, r := range batch {
				uids = append(uids, r.UID)
			}
			hdrs, err := cl.FetchThreadHeaders(uids)
			if err != nil {
				log.Printf("thread repair account %d: fetch %s: %v", acct.ID, fname, err)
				break
			}
			for _, r := range batch {
				th, ok := hdrs[r.UID]
				if !ok || (len(th.References) == 0 && th.InReplyTo == "") {
					continue
				}
				if err := m.st.RecomputeThreadKey(acct.ID, r.ID, th.References, th.InReplyTo); err != nil {
					log.Printf("thread repair account %d: recompute %d: %v", acct.ID, r.ID, err)
					continue
				}
				fixed++
			}
		}
	}
	if fixed > 0 {
		log.Printf("thread repair account %d: %d messages re-keyed from References", acct.ID, fixed)
		m.emit(Change{AccountID: acct.ID})
	}
}
