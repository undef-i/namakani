package proton

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/hydroxide/protonmail"

	"namakani/internal/mail"
)

type folderCache struct {
	name    string
	label   string
	ids     []string
	meta    map[string]*protonmail.Message
	uidByID map[string]uint32
	idByUID map[uint32]string
	maxUID  uint32
	total   int
}

func (s *Session) Close() {}

func (s *Session) Capabilities() []string { return []string{"PROTON-API"} }

func (s *Session) HasCap(cap imap.Cap) bool { return false }

func (s *Session) ListFolders() ([]mail.FolderInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mail.FolderInfo, 0, len(systemFolders)+4)
	for _, f := range systemFolders {
		out = append(out, mail.FolderInfo{Name: f.name, SpecialUse: f.special})
	}
	labels, err := s.c.ListLabels()
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		if systemLabelIDs[l.ID] {
			continue
		}
		out = append(out, mail.FolderInfo{Name: l.Name})
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

func (s *Session) probeFolderLocked(fc *folderCache) (bool, error) {
	total, msgs, err := s.c.ListMessages(&protonmail.MessageFilter{
		Label: fc.label, Page: 0, PageSize: 150, Asc: false,
	})
	if err != nil {
		return false, err
	}
	if total != fc.total {
		return false, nil
	}
	for _, mm := range msgs {
		if _, ok := fc.meta[mm.ID]; ok {
			fc.meta[mm.ID] = mm
		}
	}
	if len(msgs) == 0 {
		return fc.total == 0, nil
	}
	if len(fc.ids) == 0 || msgs[0].ID != fc.ids[len(fc.ids)-1] {
		return false, nil
	}
	for _, mm := range msgs {
		if _, ok := fc.meta[mm.ID]; !ok {
			return false, nil
		}
	}
	return true, nil
}

func (s *Session) listFolderLocked(name string) (*folderCache, error) {
	if s.folders == nil {
		s.folders = make(map[string]*folderCache)
	}
	if fc, ok := s.folders[name]; ok {
		fresh, err := s.probeFolderLocked(fc)
		if err != nil {
			return nil, err
		}
		if fresh {
			return fc, nil
		}
	}
	label, err := s.folderLabel(name)
	if err != nil {
		return nil, err
	}
	uidMap, maxUID, err := s.st.ProtonUIDMap(s.acct.ID, name)
	if err != nil {
		return nil, err
	}
	fc := &folderCache{
		name:    name,
		label:   label,
		meta:    make(map[string]*protonmail.Message),
		uidByID: make(map[string]uint32),
		idByUID: make(map[uint32]string),
		maxUID:  maxUID,
	}
	for page := 0; ; page++ {
		total, msgs, err := s.c.ListMessages(&protonmail.MessageFilter{
			Label: label, Page: page, PageSize: 150, Asc: true,
		})
		if err != nil {
			return nil, err
		}
		fc.total = total
		for _, m := range msgs {
			uid, ok := uidMap[m.ID]
			if !ok {
				uid, err = s.st.AssignProtonUID(s.acct.ID, name, m.ID)
				if err != nil {
					return nil, err
				}
				uidMap[m.ID] = uid
				if uid > fc.maxUID {
					fc.maxUID = uid
				}
			}
			if _, dup := fc.meta[m.ID]; !dup {
				fc.ids = append(fc.ids, m.ID)
			}
			fc.meta[m.ID] = m
			fc.uidByID[m.ID] = uid
			fc.idByUID[uid] = m.ID
		}
		if len(msgs) < 150 || len(fc.ids) >= total {
			break
		}
	}
	s.folders[name] = fc
	return fc, nil
}

func (s *Session) currentLocked() (*folderCache, error) {
	if s.cur == nil {
		return nil, fmt.Errorf("proton: no folder selected")
	}
	return s.cur, nil
}

func (fc *folderCache) drop(id string) {
	uid, ok := fc.uidByID[id]
	if !ok {
		return
	}
	delete(fc.meta, id)
	delete(fc.uidByID, id)
	delete(fc.idByUID, uid)
	for i, x := range fc.ids {
		if x == id {
			fc.ids = append(fc.ids[:i], fc.ids[i+1:]...)
			break
		}
	}
	fc.total--
}

func hasLabel(m *protonmail.Message, label string) bool {
	for _, id := range m.LabelIDs {
		if id == label {
			return true
		}
	}
	return false
}

func convAddr(a *protonmail.MessageAddress) mail.Addr {
	if a == nil {
		return mail.Addr{}
	}
	return mail.Addr{Name: a.Name, Email: a.Address}
}

func convAddrs(in []*protonmail.MessageAddress) []mail.Addr {
	out := make([]mail.Addr, 0, len(in))
	for _, a := range in {
		out = append(out, convAddr(a))
	}
	return out
}

func messageIDOf(m *protonmail.Message) string {
	if m.ExternalID != "" {
		return strings.Trim(m.ExternalID, "<>")
	}
	return m.ID + "@protonmail.com"
}

func toMsg(uid uint32, m *protonmail.Message) mail.Msg {
	msg := mail.Msg{
		UID:       uid,
		MessageID: messageIDOf(m),
		Subject:   m.Subject,
		Size:      m.Size,
		Date:      m.Time.Time(),
		HasAttach: m.NumAttachments > 0,
	}
	if m.Sender != nil {
		msg.From = []mail.Addr{convAddr(m.Sender)}
	}
	msg.To = convAddrs(m.ToList)
	msg.Cc = convAddrs(m.CCList)
	if m.Unread == 0 {
		msg.Flags |= mail.FlagSeen
	}
	if hasLabel(m, protonmail.LabelStarred) {
		msg.Flags |= mail.FlagFlagged
	}
	if m.Type == protonmail.MessageDraft || hasLabel(m, protonmail.LabelDraft) {
		msg.Flags |= mail.FlagDraft
	}
	if m.IsReplied > 0 || m.IsRepliedAll > 0 {
		msg.Flags |= mail.FlagAnswered
	}
	return msg
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
		out = append(out, toMsg(uid, m))
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
		out = append(out, mail.Msg{UID: fc.uidByID[id], Flags: toMsg(0, fc.meta[id]).Flags})
	}
	return out, nil
}

func (s *Session) FetchRaw(uid uint32) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return nil, err
	}
	id, ok := fc.idByUID[uid]
	if !ok {
		return nil, fmt.Errorf("proton: message %d not found in %s", uid, fc.name)
	}
	return s.buildRawLocked(id)
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
		m, err := s.c.GetMessage(id)
		if err != nil {
			return nil, err
		}
		out[uid] = parseThreadHeaders(m.Header)
	}
	return out, nil
}

func (s *Session) flagOps(id string, flag imap.Flag, add bool) error {
	switch flag {
	case imap.FlagSeen:
		if add {
			return s.c.MarkMessagesRead([]string{id})
		}
		return s.c.MarkMessagesUnread([]string{id})
	case imap.FlagFlagged:
		if add {
			return s.c.LabelMessages(protonmail.LabelStarred, []string{id})
		}
		return s.c.UnlabelMessages(protonmail.LabelStarred, []string{id})
	}
	return fmt.Errorf("proton: unsupported flag %v", flag)
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
		return fmt.Errorf("proton: message %d not found in %s", uid, fc.name)
	}
	if err := s.flagOps(id, flag, add); err != nil {
		return err
	}
	if m, ok := fc.meta[id]; ok {
		switch flag {
		case imap.FlagSeen:
			if add {
				m.Unread = 0
			} else {
				m.Unread = 1
			}
		case imap.FlagFlagged:
			m.LabelIDs = setLabel(m.LabelIDs, protonmail.LabelStarred, add)
		}
	}
	return nil
}

func setLabel(ids []string, label string, add bool) []string {
	out := ids[:0]
	found := false
	for _, id := range ids {
		if id == label {
			found = true
			if add {
				out = append(out, id)
			}
			continue
		}
		out = append(out, id)
	}
	if add && !found {
		out = append(out, label)
	}
	return out
}

func (s *Session) StoreFlags(uids []uint32, flag imap.Flag, add bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return err
	}
	var ids []string
	for _, uid := range uids {
		if id, ok := fc.idByUID[uid]; ok {
			ids = append(ids, id)
		}
	}
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		var err error
		switch flag {
		case imap.FlagSeen:
			if add {
				err = s.c.MarkMessagesRead(chunk)
			} else {
				err = s.c.MarkMessagesUnread(chunk)
			}
		case imap.FlagFlagged:
			if add {
				err = s.c.LabelMessages(protonmail.LabelStarred, chunk)
			} else {
				err = s.c.UnlabelMessages(protonmail.LabelStarred, chunk)
			}
		default:
			return fmt.Errorf("proton: unsupported flag %v", flag)
		}
		if err != nil {
			return err
		}
	}
	for _, id := range ids {
		if m, ok := fc.meta[id]; ok {
			switch flag {
			case imap.FlagSeen:
				if add {
					m.Unread = 0
				} else {
					m.Unread = 1
				}
			case imap.FlagFlagged:
				m.LabelIDs = setLabel(m.LabelIDs, protonmail.LabelStarred, add)
			}
		}
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
		return 0, fmt.Errorf("proton: message %d not found in %s", uid, fc.name)
	}
	destLabel, err := s.folderLabel(dest)
	if err != nil {
		return 0, err
	}
	left := false
	if destLabel != fc.label {
		switch {
		case destLabel == protonmail.LabelStarred:
			if err := s.c.LabelMessages(protonmail.LabelStarred, []string{id}); err != nil {
				return 0, err
			}
		default:
			if fc.label == protonmail.LabelStarred {
				if err := s.c.UnlabelMessages(protonmail.LabelStarred, []string{id}); err != nil {
					return 0, err
				}
				left = true
			}
			if err := s.c.LabelMessages(destLabel, []string{id}); err != nil {
				return 0, err
			}
			if locationLabels[fc.label] || !systemLabelIDs[fc.label] {
				if err := s.c.UnlabelMessages(fc.label, []string{id}); err != nil {
					return 0, err
				}
				left = true
			}
		}
	}
	destUID, err := s.st.AssignProtonUID(s.acct.ID, dest, id)
	if err != nil {
		return 0, err
	}
	if left {
		fc.drop(id)
	} else if destLabel != fc.label {
		if m, ok := fc.meta[id]; ok {
			m.LabelIDs = setLabel(m.LabelIDs, destLabel, true)
		}
	}
	if dest != fc.name {
		delete(s.folders, dest)
	}
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
		return fmt.Errorf("proton: message %d not found in %s", uid, fc.name)
	}
	if err := s.c.DeleteMessages([]string{id}); err != nil {
		return err
	}
	fc.drop(id)
	return nil
}

func (s *Session) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, _, _, _, _, err := s.createMessageLocked(bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	label, err := s.folderLabel(folder)
	if err != nil {
		return 0, err
	}
	if label != protonmail.LabelDraft {
		if err := s.c.LabelMessages(label, []string{msg.ID}); err != nil {
			return 0, err
		}
	}
	uid, err := s.st.AssignProtonUID(s.acct.ID, folder, msg.ID)
	if err != nil {
		return 0, err
	}
	delete(s.folders, folder)
	return uid, nil
}

func (s *Session) AssignUID(folder, protonID string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.AssignProtonUID(s.acct.ID, folder, protonID)
}

func (s *Session) MessageCounts() (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts, err := s.c.CountMessages("")
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(counts))
	for _, c := range counts {
		out[c.LabelID] = c.Total
	}
	return out, nil
}

func (s *Session) FolderLabelPairs() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(systemFolders)+4)
	for _, f := range systemFolders {
		out[f.name] = f.label
	}
	labels, err := s.c.ListLabels()
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		if systemLabelIDs[l.ID] {
			continue
		}
		out[l.Name] = l.ID
	}
	return out, nil
}

func (s *Session) EventLatest() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, err := s.c.GetEvent("")
	if err != nil {
		return "", err
	}
	return ev.ID, nil
}

func (s *Session) EventSince(last string) (*protonmail.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.GetEvent(last)
}
