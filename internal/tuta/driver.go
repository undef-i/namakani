package tuta

import (
	"fmt"
	"net/http"
	"time"

	"github.com/emersion/go-imap/v2"

	"namakani/internal/mail"
)

const entriesPageSize = 200

type folderCache struct {
	name    string
	folder  tutaFolder
	ids     []string
	meta    map[string]*mailMeta
	uidByID map[string]uint32
	idByUID map[uint32]string
	maxUID  uint32
	total   int
}

func (s *Session) Close() {}

func (s *Session) Capabilities() []string { return []string{"TUTA-API"} }

func (s *Session) HasCap(cap imap.Cap) bool { return false }

func (s *Session) ListFolders() ([]mail.FolderInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	folders, err := s.listFolders()
	if err != nil {
		return nil, err
	}
	s.folders = folders
	out := make([]mail.FolderInfo, 0, len(folders))
	for _, f := range folders {
		out = append(out, mail.FolderInfo{Name: f.name, SpecialUse: f.special})
	}
	return out, nil
}

func (s *Session) Select(name string) (*mail.SelectData, error) { return s.selectFolder(name) }

func (s *Session) SelectRW(name string) (*mail.SelectData, error) { return s.selectFolder(name) }

func (s *Session) selectFolder(name string) (*mail.SelectData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.listFolderLocked(name)
	if err != nil {
		return nil, err
	}
	s.cur = fc
	return &mail.SelectData{UIDValidity: 1, UIDNext: fc.maxUID + 1, NumMessages: uint32(fc.total)}, nil
}

func (s *Session) listFolderLocked(name string) (*folderCache, error) {
	folder, err := s.folderByName(name)
	if err != nil {
		return nil, err
	}
	uidMap, maxUID, err := s.st.TutaUIDMap(s.acct.ID, name)
	if err != nil {
		return nil, err
	}
	fc := &folderCache{
		name:    name,
		folder:  folder,
		meta:    make(map[string]*mailMeta),
		uidByID: make(map[string]uint32),
		idByUID: make(map[uint32]string),
		maxUID:  maxUID,
	}
	start := ""
	for {
		entries, err := s.rest.loadRange("tutanota", "mailsetentry", modelVersionTutanota, folder.entriesID, start, entriesPageSize, false)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break
		}
		var fresh []*mailMeta
		byList := make(map[string][]string)
		entryMail := make(map[string][2]string)
		for _, e := range entries {
			pairs := e.idTuples("1456")
			if len(pairs) != 1 {
				continue
			}
			entryMail[e.elemID("1452")] = pairs[0]
			byList[pairs[0][0]] = append(byList[pairs[0][0]], pairs[0][1])
		}
		for listID, ids := range byList {
			mails, err := s.rest.loadMultiple("tutanota", "mail", modelVersionTutanota, listID, ids)
			if err != nil {
				return nil, err
			}
			decrypted := make(map[string]instance, len(mails))
			for _, m := range mails {
				sk, err := s.decryptInstance(m, mailSpec)
				if err != nil {
					return nil, err
				}
				if sk != nil {
					if pairs := m.idTuples("1308"); len(pairs) > 0 {
						s.cacheInstKey(pairs[0][1], sk)
					}
				}
				_, el, _ := m.idTuple("99")
				decrypted[el] = m
			}
			for _, id := range ids {
				m, ok := decrypted[id]
				if !ok {
					continue
				}
				meta := metaFromMail(m, listID)
				fc.meta[id] = meta
				if _, known := uidMap[id]; !known {
					fresh = append(fresh, meta)
				}
			}
		}
		if err := s.enrichNewMail(fresh); err != nil {
			return nil, err
		}
		for _, e := range entries {
			pair, ok := entryMail[e.elemID("1452")]
			if !ok {
				continue
			}
			id := pair[1]
			if _, ok := fc.meta[id]; !ok {
				continue
			}
			uid, known := uidMap[id]
			if !known {
				uid, err = s.st.AssignTutaUID(s.acct.ID, name, id)
				if err != nil {
					return nil, err
				}
				uidMap[id] = uid
				if uid > fc.maxUID {
					fc.maxUID = uid
				}
			}
			if _, dup := fc.uidByID[id]; !dup {
				fc.ids = append(fc.ids, id)
			}
			fc.uidByID[id] = uid
			fc.idByUID[uid] = id
		}
		fc.total = len(fc.ids)
		if len(entries) < entriesPageSize {
			break
		}
		start = entries[len(entries)-1].elemID("1452")
	}
	return fc, nil
}

func metaFromMail(m instance, listID string) *mailMeta {
	meta := &mailMeta{listID: listID}
	_, el, _ := m.idTuple("99")
	meta.id = el
	meta.subject = m.str("105")
	meta.unread = m.boolean("109")
	meta.pinned = m.boolean("110")
	meta.confid = m.boolean("426")
	if ms := m.num("107"); ms > 0 {
		meta.date = time.UnixMilli(ms)
	}
	meta.from = addrFromInstance(m.agg("111"))
	meta.firstRcpt = addrFromInstance(m.agg("1306"))
	meta.attachIDs = m.idTuples("115")
	if pairs := m.idTuples("1308"); len(pairs) > 0 {
		meta.detailsArc = pairs[0][0]
		meta.detailsID = pairs[0][1]
		meta.hasDetails = true
	}
	if meta.messageID == "" {
		meta.messageID = meta.id + "@tuta"
	}
	return meta
}

func (s *Session) enrichNewMail(fresh []*mailMeta) error {
	byArchive := make(map[string][]*mailMeta)
	for _, m := range fresh {
		if m.hasDetails {
			byArchive[m.detailsArc] = append(byArchive[m.detailsArc], m)
		}
	}
	for arc, metas := range byArchive {
		ids := make([]string, 0, len(metas))
		byID := make(map[string]*mailMeta, len(metas))
		for _, m := range metas {
			ids = append(ids, m.detailsID)
			byID[m.detailsID] = m
		}
		blobs, err := s.loadBlobElements(arc, ids)
		if err != nil {
			return err
		}
		for _, blob := range blobs {
			if _, err := s.decryptInstance(blob, mailDetailsBlobSpec); err != nil {
				return err
			}
			_, el, _ := blob.idTuple("1300")
			m := byID[el]
			if m == nil {
				continue
			}
			applyDetails(m, blob.agg("1305"))
		}
	}
	return nil
}

func applyDetails(m *mailMeta, details instance) {
	if details == nil {
		return
	}
	if rcpts := details.agg("1286"); rcpts != nil {
		m.to = addrsFromAggList(rcpts, "1279")
		m.cc = addrsFromAggList(rcpts, "1280")
	}
	if hdr := details.agg("1287"); hdr != nil {
		text := hdr.str("1271")
		if text == "" {
			text = hdr.str("1272")
		}
		if text != "" {
			mid, refs, irt := parseHeadersText(text)
			if mid != "" {
				m.messageID = mid
			}
			m.references = refs
			m.inReplyTo = irt
		}
	}
}

func (s *Session) currentLocked() (*folderCache, error) {
	if s.cur == nil {
		return nil, fmt.Errorf("tuta: no folder selected")
	}
	return s.cur, nil
}

func (s *Session) FetchEnvelopes(uids []uint32) ([]mail.Msg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return nil, err
	}
	out := make([]mail.Msg, 0, len(uids))
	for _, uid := range uids {
		id, ok := fc.idByUID[uid]
		if !ok {
			continue
		}
		m, ok := fc.meta[id]
		if !ok {
			continue
		}
		out = append(out, m.toMsg(uid, fc.folder.kind))
	}
	return out, nil
}

func (s *Session) FetchFlags(changedSince uint64) ([]mail.Msg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return nil, err
	}
	out := make([]mail.Msg, 0, len(fc.ids))
	for _, id := range fc.ids {
		out = append(out, mail.Msg{UID: fc.uidByID[id], Flags: fc.meta[id].toMsg(0, fc.folder.kind).Flags})
	}
	return out, nil
}

const detailsCacheMax = 256

func (s *Session) PrefetchBodies(uids []uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return err
	}
	byArchive := make(map[string][]*mailMeta)
	for _, uid := range uids {
		id, ok := fc.idByUID[uid]
		if !ok {
			continue
		}
		m := fc.meta[id]
		if !m.hasDetails {
			continue
		}
		if _, ok := s.details[m.id]; ok {
			continue
		}
		byArchive[m.detailsArc] = append(byArchive[m.detailsArc], m)
	}
	for arc, metas := range byArchive {
		ids := make([]string, 0, len(metas))
		byID := make(map[string]*mailMeta, len(metas))
		for _, m := range metas {
			ids = append(ids, m.detailsID)
			byID[m.detailsID] = m
		}
		blobs, err := s.loadBlobElements(arc, ids)
		if err != nil {
			return err
		}
		for _, blob := range blobs {
			if _, err := s.decryptInstance(blob, mailDetailsBlobSpec); err != nil {
				return err
			}
			_, el, _ := blob.idTuple("1300")
			m := byID[el]
			if m == nil {
				continue
			}
			if s.details == nil {
				s.details = make(map[string]instance)
			}
			if len(s.details) >= detailsCacheMax {
				clear(s.details)
			}
			s.details[m.id] = blob.agg("1305")
		}
	}
	return nil
}

func (s *Session) loadDetails(m *mailMeta) (instance, error) {
	if !m.hasDetails {
		return nil, nil
	}
	if d, ok := s.details[m.id]; ok {
		delete(s.details, m.id)
		return d, nil
	}
	blobs, err := s.loadBlobElements(m.detailsArc, []string{m.detailsID})
	if err != nil {
		return nil, err
	}
	if len(blobs) == 0 {
		return nil, fmt.Errorf("tuta: mail details blob %s not found", m.detailsID)
	}
	blob := blobs[0]
	if _, err := s.decryptInstance(blob, mailDetailsBlobSpec); err != nil {
		return nil, err
	}
	return blob.agg("1305"), nil
}

func (s *Session) FetchThreadHeaders(uids []uint32) (map[uint32]mail.ThreadHeaders, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]mail.ThreadHeaders, len(uids))
	for _, uid := range uids {
		id, ok := fc.idByUID[uid]
		if !ok {
			continue
		}
		m := fc.meta[id]
		th := mail.ThreadHeaders{References: m.references, InReplyTo: m.inReplyTo}
		if len(th.References) == 0 && th.InReplyTo == "" && m.hasDetails {
			details, err := s.loadDetails(m)
			if err != nil {
				return nil, err
			}
			if details != nil {
				if hdr := details.agg("1287"); hdr != nil {
					text := hdr.str("1271")
					if text == "" {
						text = hdr.str("1272")
					}
					_, refs, irt := parseHeadersText(text)
					th.References = refs
					th.InReplyTo = irt
				}
			}
		}
		out[uid] = th
	}
	return out, nil
}

func (s *Session) setUnread(ids [][2]string, unread bool) error {
	for start := 0; start < len(ids); start += 50 {
		end := start + 50
		if end > len(ids) {
			end = len(ids)
		}
		tuples := make([]any, 0, end-start)
		for _, pair := range ids[start:end] {
			tuples = append(tuples, []any{pair[0], pair[1]})
		}
		val := "0"
		if unread {
			val = "1"
		}
		_, err := s.rest.callService(http.MethodPost, "tutanota", "unreadmailstateservice", modelVersionTutanota, instance{
			"1475": "0",
			"1476": tuples,
			"1477": val,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) setPinned(m *mailMeta, pinned bool) error {
	raw, err := s.rest.getEntity("tutanota", "mail", modelVersionTutanota, m.listID, m.id)
	if err != nil {
		return err
	}
	val := "0"
	if pinned {
		val = "1"
	}
	raw["110"] = val
	return s.rest.updateEntity("tutanota", "mail", modelVersionTutanota, []string{m.listID, m.id}, raw)
}

func (s *Session) StoreFlag(uid uint32, flag imap.Flag, add bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return err
	}
	id, ok := fc.idByUID[uid]
	if !ok {
		return fmt.Errorf("tuta: message %d not found in %s", uid, fc.name)
	}
	m := fc.meta[id]
	switch flag {
	case imap.FlagSeen:
		if err := s.setUnread([][2]string{{m.listID, m.id}}, !add); err != nil {
			return err
		}
		m.unread = !add
	case imap.FlagFlagged:
		if err := s.setPinned(m, add); err != nil {
			return err
		}
		m.pinned = add
	default:
		return fmt.Errorf("tuta: unsupported flag %v", flag)
	}
	return nil
}

func (s *Session) StoreFlags(uids []uint32, flag imap.Flag, add bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return err
	}
	var metas []*mailMeta
	for _, uid := range uids {
		if id, ok := fc.idByUID[uid]; ok {
			metas = append(metas, fc.meta[id])
		}
	}
	switch flag {
	case imap.FlagSeen:
		pairs := make([][2]string, 0, len(metas))
		for _, m := range metas {
			pairs = append(pairs, [2]string{m.listID, m.id})
		}
		if err := s.setUnread(pairs, !add); err != nil {
			return err
		}
		for _, m := range metas {
			m.unread = !add
		}
	case imap.FlagFlagged:
		for _, m := range metas {
			if err := s.setPinned(m, add); err != nil {
				return err
			}
			m.pinned = add
		}
	default:
		return fmt.Errorf("tuta: unsupported flag %v", flag)
	}
	return nil
}

func (s *Session) MoveMessage(uid uint32, dest string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return 0, err
	}
	id, ok := fc.idByUID[uid]
	if !ok {
		return 0, fmt.Errorf("tuta: message %d not found in %s", uid, fc.name)
	}
	m := fc.meta[id]
	destFolder, err := s.folderByName(dest)
	if err != nil {
		return 0, err
	}
	_, err = s.rest.callService(http.MethodPost, "tutanota", "movemailservice", modelVersionTutanota, instance{
		"446":  "0",
		"447":  []any{destFolder.setListID, destFolder.setID},
		"448":  []any{[]any{m.listID, m.id}},
		"1644": []any{},
		"1714": nil,
	})
	if err != nil {
		return 0, err
	}
	destUID, err := s.st.AssignTutaUID(s.acct.ID, dest, id)
	if err != nil {
		return 0, err
	}
	s.cur = nil
	return destUID, nil
}

func (s *Session) DeleteMessage(uid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return err
	}
	id, ok := fc.idByUID[uid]
	if !ok {
		return fmt.Errorf("tuta: message %d not found in %s", uid, fc.name)
	}
	m := fc.meta[id]
	if err := s.rest.eraseEntity("tutanota", "mail", modelVersionTutanota, m.listID, m.id); err != nil {
		return err
	}
	s.cur = nil
	return nil
}

func (s *Session) AssignUID(folder, tutaID string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.AssignTutaUID(s.acct.ID, folder, tutaID)
}
