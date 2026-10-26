package sync

import (
	"context"
	"log"
	"os"
	"sort"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func bgTurn(ctx context.Context, cl mail.Conn) (func(), bool) {
	if t, ok := cl.(mail.BackgroundTurner); ok {
		return t.BackgroundTurn(ctx)
	}
	if ctx.Err() != nil {
		return nil, false
	}
	return func() {}, true
}

func (m *Manager) prewarm(ctx context.Context, cl mail.Conn, acct mail.Account) {
	batch := m.st.AppSettings().SyncBatch
	attBatch := batch / 2
	if attBatch < 1 {
		attBatch = 1
	}
	room, err := m.st.NewCacheRoom()
	if err != nil {
		log.Printf("prewarm account %d: cache room: %v", acct.ID, err)
		return
	}
	sel := ""
	for {
		if ctx.Err() != nil {
			return
		}
		cands, err := m.st.BodyCandidates(acct.ID, batch)
		if err != nil {
			log.Printf("prewarm account %d: candidates: %v", acct.ID, err)
			return
		}
		if len(cands) == 0 {
			break
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].FolderName < cands[j].FolderName })
		progressed := false
		for i := 0; i < len(cands); {
			j := i
			for j < len(cands) && cands[j].FolderName == cands[i].FolderName {
				j++
			}
			group := cands[i:j]
			if _, ok := cl.(mail.BodyPrefetcher); ok {
				end, ok := bgTurn(ctx, cl)
				if !ok {
					return
				}
				if sel != group[0].FolderName {
					if _, err := cl.Select(group[0].FolderName); err != nil {
						log.Printf("prewarm account %d: select %s: %v", acct.ID, group[0].FolderName, err)
						end()
						return
					}
					sel = group[0].FolderName
				}
				uids := make([]uint32, 0, len(group))
				for _, c := range group {
					uids = append(uids, c.UID)
				}
				if err := cl.(mail.BodyPrefetcher).PrefetchBodies(uids); err != nil {
					log.Printf("prewarm account %d: prefetch %s: %v", acct.ID, group[0].FolderName, err)
				}
				end()
			}
			for _, c := range group {
				if ctx.Err() != nil {
					return
				}
				end, ok := bgTurn(ctx, cl)
				if !ok {
					return
				}
				okc := m.cacheBody(cl, acct, c, &sel, room)
				end()
				if okc {
					progressed = true
				}
			}
			i = j
		}
		if !progressed || len(cands) < batch {
			break
		}
	}
	remaining, err := m.st.BodyCandidates(acct.ID, 1)
	if err != nil || len(remaining) > 0 {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		atts, err := m.st.AttachmentCandidates(acct.ID, attBatch)
		if err != nil {
			log.Printf("prewarm account %d: attachment candidates: %v", acct.ID, err)
			return
		}
		if len(atts) == 0 {
			return
		}
		sort.SliceStable(atts, func(i, j int) bool { return atts[i].FolderName < atts[j].FolderName })
		progressed := false
		for _, c := range atts {
			if ctx.Err() != nil {
				return
			}
			end, ok := bgTurn(ctx, cl)
			if !ok {
				return
			}
			okc := m.cacheAttachment(cl, acct, c, &sel, room)
			end()
			if okc {
				progressed = true
			}
		}
		if !progressed || len(atts) < attBatch {
			return
		}
	}
}

func (m *Manager) cacheBody(cl mail.Conn, acct mail.Account, c store.BodyCandidate, sel *string, room *store.CacheRoom) bool {
	if *sel != c.FolderName {
		selData, err := cl.Select(c.FolderName)
		if err != nil {
			log.Printf("prewarm account %d: select %s: %v", acct.ID, c.FolderName, err)
			return false
		}
		*sel = c.FolderName
		if state, serr := m.st.FolderState(c.FolderID); serr == nil && state.UIDValidity != 0 && selData.UIDValidity != 0 && selData.UIDValidity != state.UIDValidity {
			return false
		}
	}
	raw, err := cl.FetchRaw(c.UID)
	if err != nil {
		log.Printf("prewarm account %d: fetch %s/%d: %v", acct.ID, c.FolderName, c.UID, err)
		return false
	}
	parsed, err := mail.ParseBody(raw)
	if err != nil {
		log.Printf("prewarm account %d: parse %s/%d: %v", acct.ID, c.FolderName, c.UID, err)
		return false
	}
	size := int64(len(parsed.Text) + len(parsed.HTML))
	fits, err := room.MakeRoomForBody(size, c.DateTS, c.MessageID)
	if err != nil {
		log.Printf("prewarm account %d: budget: %v", acct.ID, err)
		return false
	}
	if !fits {
		return false
	}
	if err := m.st.SaveBody(c.MessageID, parsed.Text, parsed.HTML, mail.MakeSnippet(parsed.Text, parsed.HTML), parsed.Truncated); err != nil {
		log.Printf("prewarm account %d: save body %d: %v", acct.ID, c.MessageID, err)
		return false
	}
	room.NoteBodySaved(size)
	rows := make([]store.AttachmentRow, 0, len(parsed.Attachments))
	for _, a := range parsed.Attachments {
		rows = append(rows, store.AttachmentRow{
			PartPath: a.PartPath, Filename: a.Filename, Mime: a.Mime,
			Size: a.Size, ContentID: a.ContentID, Inline: a.Inline,
		})
	}
	if _, err := m.st.ReplaceAttachments(c.MessageID, rows); err != nil {
		log.Printf("prewarm account %d: attachments %d: %v", acct.ID, c.MessageID, err)
	}
	if len(parsed.References) > 0 || parsed.InReplyTo != "" {
		if err := m.st.RecomputeThreadKey(acct.ID, c.MessageID, parsed.References, parsed.InReplyTo); err != nil {
			log.Printf("prewarm account %d: thread key %d: %v", acct.ID, c.MessageID, err)
		}
	}
	return true
}

func (m *Manager) cacheAttachment(cl mail.Conn, acct mail.Account, c store.AttachmentCandidate, sel *string, room *store.CacheRoom) bool {
	if *sel != c.FolderName {
		if _, err := cl.Select(c.FolderName); err != nil {
			return false
		}
		*sel = c.FolderName
	}
	raw, err := cl.FetchRaw(c.UID)
	if err != nil {
		return false
	}
	part, err := mail.ParseBodyPart(raw, c.PartPath)
	if err != nil {
		return false
	}
	size := int64(len(part.Bytes))
	fits, err := room.MakeRoomForAttachment(size, c.DateTS, c.MessageID, c.AttachmentID)
	if err != nil || !fits {
		return false
	}
	if err := os.MkdirAll(m.st.AttachmentsDir(), 0o700); err != nil {
		log.Printf("prewarm account %d: mkdir: %v", acct.ID, err)
		return false
	}
	path := m.st.AttachmentFilePath(c.AttachmentID)
	if err := os.WriteFile(path, part.Bytes, 0o600); err != nil {
		log.Printf("prewarm account %d: write attachment %d: %v", acct.ID, c.MessageID, err)
		return false
	}
	if err := m.st.SetAttachmentLocalPath(c.AttachmentID, path); err != nil {
		_ = os.Remove(path)
		return false
	}
	room.NoteAttachmentSaved(size)
	return true
}
