package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	stdmail "net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

const allMailName = "All Mail"
const allMailQuery = "-in:spam -in:trash"

type folderDef struct {
	name    string
	special string
	labelID string
}

var systemLabelDefs = map[string]folderDef{
	"INBOX":     {name: "INBOX", special: "inbox", labelID: "INBOX"},
	"SENT":      {name: "Sent", special: "sent", labelID: "SENT"},
	"DRAFT":     {name: "Drafts", special: "drafts", labelID: "DRAFT"},
	"SPAM":      {name: "Spam", special: "junk", labelID: "SPAM"},
	"TRASH":     {name: "Trash", special: "trash", labelID: "TRASH"},
	"STARRED":   {name: "Starred", special: "flagged", labelID: "STARRED"},
	"IMPORTANT": {name: "Important", special: "", labelID: "IMPORTANT"},
}

func folderDefsFromLabels(labels []Label) []folderDef {
	out := make([]folderDef, 0, len(labels)+1)
	seen := map[string]bool{}
	var user []folderDef
	for _, l := range labels {
		if def, ok := systemLabelDefs[l.ID]; ok {
			if !seen[def.name] {
				seen[def.name] = true
				out = append(out, def)
			}
			continue
		}
		if l.Type != "user" {
			continue
		}
		name := l.Name
		if name == "" {
			name = l.ID
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		user = append(user, folderDef{name: name, labelID: l.ID})
	}
	sort.Slice(user, func(i, j int) bool { return user[i].name < user[j].name })
	out = append(out, folderDef{name: allMailName, special: "all"})
	out = append(out, user...)
	return out
}

func Probe(acct mail.Account) (int, error) {
	creds, err := ParseCreds(acct.Password)
	if err != nil {
		return 0, err
	}
	labels, err := NewClient(creds).ListLabels(context.Background())
	if err != nil {
		return 0, err
	}
	return len(folderDefsFromLabels(labels)), nil
}

type folderCache struct {
	def       folderDef
	ids       []string
	uidByID   map[string]uint32
	idByUID   map[uint32]string
	flagsByID map[string]uint32
	maxUID    uint32
}

func newFolderCache(def folderDef) *folderCache {
	return &folderCache{
		def:       def,
		uidByID:   map[string]uint32{},
		idByUID:   map[uint32]string{},
		flagsByID: map[string]uint32{},
	}
}

func (fc *folderCache) add(id string, uid uint32, flags uint32) {
	if _, ok := fc.uidByID[id]; !ok {
		fc.ids = append(fc.ids, id)
		fc.uidByID[id] = uid
		fc.idByUID[uid] = id
		if uid > fc.maxUID {
			fc.maxUID = uid
		}
	}
	fc.flagsByID[id] = flags
}

func (fc *folderCache) remove(id string) {
	uid, ok := fc.uidByID[id]
	if !ok {
		return
	}
	delete(fc.uidByID, id)
	delete(fc.idByUID, uid)
	delete(fc.flagsByID, id)
	for i, x := range fc.ids {
		if x == id {
			fc.ids = append(fc.ids[:i], fc.ids[i+1:]...)
			break
		}
	}
}

func flagsOf(m *MessageMeta) uint32 {
	var fl uint32
	if !m.HasLabel("UNREAD") {
		fl |= mail.FlagSeen
	}
	if m.HasLabel("STARRED") {
		fl |= mail.FlagFlagged
	}
	if m.HasLabel("DRAFT") {
		fl |= mail.FlagDraft
	}
	return fl
}

func folderHas(def folderDef, m *MessageMeta) bool {
	if def.labelID == "" {
		return !m.HasLabel("SPAM") && !m.HasLabel("TRASH")
	}
	return m.HasLabel(def.labelID)
}

type Session struct {
	st   *store.Store
	acct mail.Account
	cl   *Client
	ctx  context.Context

	mu            sync.Mutex
	folders       []folderDef
	defByName     map[string]folderDef
	resolved      bool
	fullMode      bool
	fullReconcile bool
	pendingHID    string
	changes       map[string]*MessageMeta
	meta          map[string]*MessageMeta
	caches        map[string]*folderCache
	cur           *folderCache

	tmu     sync.Mutex
	tcond   *sync.Cond
	tholder int
	tusers  int
}

func Connect(st *store.Store, acct mail.Account) (*Session, error) {
	creds, err := ParseCreds(acct.Password)
	if err != nil {
		return nil, err
	}
	return &Session{
		st:     st,
		acct:   acct,
		cl:     NewClient(creds),
		ctx:    context.Background(),
		meta:   map[string]*MessageMeta{},
		caches: map[string]*folderCache{},
	}, nil
}

func (s *Session) Close() {}

func (s *Session) Capabilities() []string { return []string{"GMAIL-API"} }

func (s *Session) HasCap(cap imap.Cap) bool { return false }

func (s *Session) InvalidateCaches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = false
	s.caches = map[string]*folderCache{}
	s.cur = nil
}

func (s *Session) HistoryMark() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingHID
}

func (s *Session) AckHistory(mark string) error {
	if mark == "" {
		return nil
	}
	return s.st.SetGmailHistoryID(s.acct.ID, mark)
}

func (s *Session) resolveLocked() error {
	if s.resolved {
		return nil
	}
	labels, err := s.cl.ListLabels(s.ctx)
	if err != nil {
		return err
	}
	s.folders = folderDefsFromLabels(labels)
	s.defByName = make(map[string]folderDef, len(s.folders))
	for _, d := range s.folders {
		s.defByName[d.name] = d
	}
	stored, err := s.st.GmailHistoryID(s.acct.ID)
	if err != nil {
		return err
	}
	profile, err := s.cl.Profile(s.ctx)
	if err != nil {
		return err
	}
	if stored == "" {
		s.fullMode = true
		s.pendingHID = profile.HistoryID
		s.resolved = true
		return nil
	}
	affected := map[string]bool{}
	token := ""
	finalHID := ""
	for {
		records, hid, next, err := s.cl.ListHistory(s.ctx, stored, token)
		if err != nil {
			if IsNotFound(err) {
				s.fullMode = true
				s.fullReconcile = true
				s.pendingHID = profile.HistoryID
				s.resolved = true
				return nil
			}
			return err
		}
		finalHID = hid
		for _, rec := range records {
			for _, id := range rec.Added {
				affected[id] = true
			}
			for _, id := range rec.Deleted {
				affected[id] = true
			}
			for _, ch := range rec.LabelsAdded {
				affected[ch.MessageID] = true
			}
			for _, ch := range rec.LabelsRemoved {
				affected[ch.MessageID] = true
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	changes := make(map[string]*MessageMeta, len(affected))
	for id := range affected {
		meta, err := s.cl.GetMeta(s.ctx, id)
		if err != nil {
			if IsNotFound(err) {
				changes[id] = nil
				continue
			}
			return err
		}
		changes[id] = meta
		s.meta[id] = meta
	}
	s.changes = changes
	s.pendingHID = finalHID
	s.resolved = true
	return nil
}

func (s *Session) ListFolders() ([]mail.FolderInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resolveLocked(); err != nil {
		return nil, err
	}
	out := make([]mail.FolderInfo, 0, len(s.folders))
	for _, d := range s.folders {
		out = append(out, mail.FolderInfo{Name: d.name, SpecialUse: d.special})
	}
	return out, nil
}

func (s *Session) metaLocked(id string) (*MessageMeta, error) {
	if m, ok := s.meta[id]; ok {
		return m, nil
	}
	m, err := s.cl.GetMeta(s.ctx, id)
	if err != nil {
		return nil, err
	}
	s.meta[id] = m
	return m, nil
}

func (s *Session) buildFullLocked(def folderDef) (*folderCache, error) {
	var ids []string
	var err error
	if def.labelID == "" {
		ids, err = s.cl.ListAllMessages(s.ctx, nil, allMailQuery)
	} else {
		ids, err = s.cl.ListAllMessages(s.ctx, []string{def.labelID}, "")
	}
	if err != nil {
		return nil, err
	}
	var stored map[string]store.GmailFolderRow
	if !s.fullReconcile {
		snap, err := s.st.GmailFolderSnapshot(s.acct.ID, def.name)
		if err != nil {
			return nil, err
		}
		stored = make(map[string]store.GmailFolderRow, len(snap))
		for _, row := range snap {
			stored[row.GmailID] = row
		}
	}
	fc := newFolderCache(def)
	for _, id := range ids {
		if row, ok := stored[id]; ok && row.HasRow {
			fc.add(id, row.UID, row.Flags)
			continue
		}
		meta, err := s.metaLocked(id)
		if err != nil {
			if IsNotFound(err) {
				continue
			}
			return nil, err
		}
		uid, err := s.st.AssignGmailUID(s.acct.ID, def.name, id)
		if err != nil {
			return nil, err
		}
		fc.add(id, uid, flagsOf(meta))
	}
	return fc, nil
}

func (s *Session) buildHistoryLocked(def folderDef) (*folderCache, error) {
	snap, err := s.st.GmailFolderSnapshot(s.acct.ID, def.name)
	if err != nil {
		return nil, err
	}
	for _, row := range snap {
		if !row.HasRow {
			if _, touched := s.changes[row.GmailID]; !touched {
				return s.buildFullLocked(def)
			}
		}
	}
	fc := newFolderCache(def)
	for _, row := range snap {
		fc.add(row.GmailID, row.UID, row.Flags)
	}
	for id, meta := range s.changes {
		if meta == nil {
			fc.remove(id)
			continue
		}
		if !folderHas(def, meta) {
			fc.remove(id)
			continue
		}
		uid := fc.uidByID[id]
		if uid == 0 {
			var err error
			uid, err = s.st.AssignGmailUID(s.acct.ID, def.name, id)
			if err != nil {
				return nil, err
			}
		}
		fc.add(id, uid, flagsOf(meta))
	}
	return fc, nil
}

func (s *Session) selectLocked(name string) (*mail.SelectData, error) {
	if err := s.resolveLocked(); err != nil {
		return nil, err
	}
	def, ok := s.defByName[name]
	if !ok {
		return nil, fmt.Errorf("gmail: folder %s not found", name)
	}
	fc, ok := s.caches[name]
	if !ok {
		var err error
		if s.fullMode {
			fc, err = s.buildFullLocked(def)
		} else {
			fc, err = s.buildHistoryLocked(def)
		}
		if err != nil {
			return nil, err
		}
		s.caches[name] = fc
	}
	s.cur = fc
	return &mail.SelectData{UIDValidity: 1, UIDNext: fc.maxUID + 1, NumMessages: uint32(len(fc.ids))}, nil
}

func (s *Session) Select(name string) (*mail.SelectData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectLocked(name)
}

func (s *Session) SelectRW(name string) (*mail.SelectData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectLocked(name)
}

func (s *Session) currentLocked() (*folderCache, error) {
	if s.cur == nil {
		return nil, fmt.Errorf("gmail: no folder selected")
	}
	return s.cur, nil
}

func parseAddrList(v string) []mail.Addr {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	list, err := (&stdmail.AddressParser{}).ParseList(v)
	if err != nil {
		return []mail.Addr{{Email: strings.TrimSpace(v)}}
	}
	out := make([]mail.Addr, 0, len(list))
	for _, a := range list {
		out = append(out, mail.Addr{Name: a.Name, Email: a.Address})
	}
	return out
}

func (m *MessageMeta) toMsg(uid uint32) mail.Msg {
	mid := m.MessageID
	if mid == "" {
		mid = m.ID + "@gmail"
	}
	date := time.UnixMilli(m.InternalDate)
	if m.DateHeader != "" {
		if t, err := stdmail.ParseDate(m.DateHeader); err == nil {
			date = t
		}
	}
	return mail.Msg{
		UID:       uid,
		MessageID: mid,
		InReplyTo: m.InReplyTo,
		Subject:   m.Subject,
		From:      parseAddrList(m.From),
		To:        parseAddrList(m.To),
		Cc:        parseAddrList(m.Cc),
		Date:      date,
		Flags:     flagsOf(m),
		Size:      m.Size,
		HasAttach: m.HasAttach,
	}
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
		if s.fullMode && !s.fullReconcile {
			if _, known := s.meta[id]; !known {
				env, found, err := s.st.GmailStoredEnvelope(s.acct.ID, id)
				if err != nil {
					return nil, err
				}
				if found {
					out = append(out, storedMsg(env, id, uid))
					continue
				}
			}
		}
		meta, err := s.metaLocked(id)
		if err != nil {
			if IsNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, meta.toMsg(uid))
	}
	return out, nil
}

func storedMsg(env store.GmailEnvelope, id string, uid uint32) mail.Msg {
	var from, to, cc []mail.Addr
	_ = json.Unmarshal([]byte(env.FromJSON), &from)
	_ = json.Unmarshal([]byte(env.ToJSON), &to)
	_ = json.Unmarshal([]byte(env.CcJSON), &cc)
	mid := env.MessageID
	if mid == "" {
		mid = id + "@gmail"
	}
	inReplyTo := ""
	if env.ThreadKey != "" && env.ThreadKey != mid {
		inReplyTo = env.ThreadKey
	}
	return mail.Msg{
		UID:       uid,
		MessageID: mid,
		InReplyTo: inReplyTo,
		Subject:   env.Subject,
		From:      from,
		To:        to,
		Cc:        cc,
		Date:      time.Unix(env.DateTS, 0),
		Flags:     env.Flags,
		Size:      env.Size,
		HasAttach: env.HasAttach,
	}
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
		out = append(out, mail.Msg{UID: fc.uidByID[id], Flags: fc.flagsByID[id]})
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
		return nil, fmt.Errorf("gmail: message %d not found in %s", uid, fc.def.name)
	}
	return s.cl.GetRaw(s.ctx, id)
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
		meta, err := s.metaLocked(id)
		if err != nil {
			if IsNotFound(err) {
				continue
			}
			return nil, err
		}
		out[uid] = mail.ThreadHeaders{References: meta.References, InReplyTo: meta.InReplyTo}
	}
	return out, nil
}

func flagLabelDelta(flag imap.Flag, add bool) (addL, removeL []string) {
	switch flag {
	case imap.FlagSeen:
		if add {
			removeL = []string{"UNREAD"}
		} else {
			addL = []string{"UNREAD"}
		}
	case imap.FlagFlagged:
		if add {
			addL = []string{"STARRED"}
		} else {
			removeL = []string{"STARRED"}
		}
	case imap.FlagDeleted:
		if add {
			addL = []string{"TRASH"}
			removeL = []string{"INBOX"}
		} else {
			removeL = []string{"TRASH"}
		}
	}
	return addL, removeL
}

func (s *Session) dropCachesLocked() {
	s.caches = map[string]*folderCache{}
	s.cur = nil
}

func (s *Session) invalidateOthersLocked(keep *folderCache) {
	s.caches = map[string]*folderCache{}
	if keep != nil {
		s.caches[keep.def.name] = keep
		s.cur = keep
	} else {
		s.cur = nil
	}
}

func (fc *folderCache) refreshLocked(s *Session, id string) {
	meta, ok := s.meta[id]
	if !ok {
		return
	}
	if !folderHas(fc.def, meta) {
		fc.remove(id)
		return
	}
	if _, ok := fc.uidByID[id]; ok {
		fc.flagsByID[id] = flagsOf(meta)
	}
}

func (s *Session) applyModifyLocked(id string, addL, removeL []string) error {
	meta, err := s.cl.Modify(s.ctx, id, addL, removeL)
	if err != nil {
		return err
	}
	s.meta[id] = meta
	return nil
}

func (s *Session) StoreFlag(uid uint32, flag imap.Flag, add bool) error {
	return s.StoreFlags([]uint32{uid}, flag, add)
}

func (s *Session) StoreFlags(uids []uint32, flag imap.Flag, add bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return err
	}
	addL, removeL := flagLabelDelta(flag, add)
	if len(addL) == 0 && len(removeL) == 0 {
		return nil
	}
	for _, uid := range uids {
		id, ok := fc.idByUID[uid]
		if !ok {
			continue
		}
		if err := s.applyModifyLocked(id, addL, removeL); err != nil {
			return err
		}
		fc.refreshLocked(s, id)
	}
	s.invalidateOthersLocked(fc)
	return nil
}

func moveDelta(src, dst folderDef, m *MessageMeta) (add, remove []string) {
	addSet := map[string]bool{}
	removeSet := map[string]bool{}
	if dst.labelID != "" {
		addSet[dst.labelID] = true
	} else {
		removeSet["INBOX"] = true
		removeSet["TRASH"] = true
		removeSet["SPAM"] = true
	}
	if src.labelID != "" {
		removeSet[src.labelID] = true
	}
	if addSet["TRASH"] {
		removeSet["INBOX"] = true
		removeSet["SPAM"] = true
	}
	if addSet["SPAM"] {
		removeSet["INBOX"] = true
		removeSet["TRASH"] = true
	}
	for l := range removeSet {
		if !m.HasLabel(l) {
			delete(removeSet, l)
		}
	}
	for l := range addSet {
		if m.HasLabel(l) {
			delete(addSet, l)
		}
		delete(removeSet, l)
	}
	for l := range addSet {
		add = append(add, l)
	}
	for l := range removeSet {
		remove = append(remove, l)
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

func (s *Session) MoveMessage(uid uint32, dest string) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return 0, err
	}
	dst, ok := s.defByName[dest]
	if !ok {
		return 0, fmt.Errorf("gmail: folder %s not found", dest)
	}
	id, ok := fc.idByUID[uid]
	if !ok {
		return 0, fmt.Errorf("gmail: message %d not found in %s", uid, fc.def.name)
	}
	meta, err := s.metaLocked(id)
	if err != nil {
		return 0, err
	}
	add, remove := moveDelta(fc.def, dst, meta)
	if len(add) > 0 || len(remove) > 0 {
		if err := s.applyModifyLocked(id, add, remove); err != nil {
			return 0, err
		}
	}
	destUID, err := s.st.AssignGmailUID(s.acct.ID, dest, id)
	if err != nil {
		return 0, err
	}
	if dst.name == fc.def.name {
		fc.refreshLocked(s, id)
	} else {
		fc.remove(id)
	}
	s.invalidateOthersLocked(fc)
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
		return fmt.Errorf("gmail: message %d not found in %s", uid, fc.def.name)
	}
	if err := s.cl.Delete(s.ctx, id); err != nil {
		return err
	}
	delete(s.meta, id)
	fc.remove(id)
	s.invalidateOthersLocked(fc)
	return nil
}

func (s *Session) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resolveLocked(); err != nil {
		return 0, err
	}
	def, ok := s.defByName[folder]
	if !ok || def.labelID != "DRAFT" {
		return 0, fmt.Errorf("gmail: messages can only be appended as drafts")
	}
	id, err := s.cl.CreateDraft(s.ctx, raw)
	if err != nil {
		return 0, err
	}
	uid, err := s.st.AssignGmailUID(s.acct.ID, folder, id)
	if err != nil {
		return 0, err
	}
	s.dropCachesLocked()
	return uid, nil
}

func (s *Session) SendRaw(raw []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cl.Send(s.ctx, raw)
}

func (s *Session) AssignUID(folder, id string) (uint32, error) {
	return s.st.AssignGmailUID(s.acct.ID, folder, id)
}
