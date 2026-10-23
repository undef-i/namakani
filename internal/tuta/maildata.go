package tuta

import (
	"fmt"
	"strings"
	"time"

	"namakani/internal/mail"
)

var mailAddressSpec = &typeSpec{
	typ:   typeID{"tutanota", 92},
	idKey: "93",
	fields: []fieldSpec{
		{id: "94", kind: "String", enc: true},
		{id: "95", kind: "String"},
	},
}

var encryptedMailAddressSpec = &typeSpec{
	typ:   typeID{"tutanota", 612},
	idKey: "613",
	fields: []fieldSpec{
		{id: "614", kind: "String", enc: true},
		{id: "615", kind: "String", enc: true},
	},
}

var mailSpec = &typeSpec{
	typ:            typeID{"tutanota", 97},
	idKey:          "99",
	ownerGroup:     "587",
	ownerEncSK:     "102",
	ownerKeyVer:    "1395",
	kdfNonce:       "1839",
	bucketKeyAssoc: "1310",
	fields: []fieldSpec{
		{id: "105", kind: "String", enc: true},
		{id: "426", kind: "Boolean", enc: true},
		{id: "617", kind: "String", enc: true},
		{id: "111", agg: mailAddressSpec},
		{id: "1306", agg: mailAddressSpec},
	},
}

var bodySpec = &typeSpec{
	typ:   typeID{"tutanota", 1273},
	idKey: "1274",
	fields: []fieldSpec{
		{id: "1275", kind: "String", enc: true},
		{id: "1276", kind: "CompressedString", enc: true},
	},
}

var headerSpec = &typeSpec{
	typ:   typeID{"tutanota", 1269},
	idKey: "1270",
	fields: []fieldSpec{
		{id: "1271", kind: "String", enc: true},
		{id: "1272", kind: "CompressedString", enc: true},
	},
}

var recipientsSpec = &typeSpec{
	typ:   typeID{"tutanota", 1277},
	idKey: "1278",
	fields: []fieldSpec{
		{id: "1279", agg: mailAddressSpec},
		{id: "1280", agg: mailAddressSpec},
		{id: "1281", agg: mailAddressSpec},
	},
}

var mailDetailsSpec = &typeSpec{
	typ:   typeID{"tutanota", 1282},
	idKey: "1283",
	fields: []fieldSpec{
		{id: "1285", agg: encryptedMailAddressSpec},
		{id: "1286", agg: recipientsSpec},
		{id: "1287", agg: headerSpec},
		{id: "1288", agg: bodySpec},
	},
}

var mailDetailsBlobSpec = &typeSpec{
	typ:         typeID{"tutanota", 1298},
	idKey:       "1300",
	ownerGroup:  "1303",
	ownerEncSK:  "1304",
	ownerKeyVer: "1408",
	kdfNonce:    "1833",
	permissions: "1301",
	fields: []fieldSpec{
		{id: "1305", agg: mailDetailsSpec},
	},
}

var blobRefSpec = &typeSpec{
	typ:   typeID{"sys", 1882},
	idKey: "1883",
}

var fileSpec = &typeSpec{
	typ:         typeID{"tutanota", 13},
	idKey:       "15",
	ownerGroup:  "580",
	ownerEncSK:  "18",
	ownerKeyVer: "1391",
	kdfNonce:    "1831",
	fields: []fieldSpec{
		{id: "21", kind: "String", enc: true},
		{id: "23", kind: "String", enc: true},
		{id: "924", kind: "String", enc: true},
		{id: "1225", agg: blobRefSpec},
	},
}

var mailSetSpec = &typeSpec{
	typ:         typeID{"tutanota", 429},
	idKey:       "431",
	ownerGroup:  "589",
	ownerEncSK:  "434",
	ownerKeyVer: "1399",
	kdfNonce:    "1847",
	fields: []fieldSpec{
		{id: "435", kind: "String", enc: true},
		{id: "1479", kind: "String", enc: true},
	},
}

type tutaFolder struct {
	name      string
	special   string
	kind      string
	groupID   string
	setListID string
	setID     string
	entriesID string
}

var systemFolderNames = map[string]struct{ name, special string }{
	"1": {"Inbox", "inbox"},
	"2": {"Sent", "sent"},
	"3": {"Trash", "trash"},
	"4": {"Archive", "archive"},
	"5": {"Spam", "junk"},
	"6": {"Drafts", "drafts"},
}

func (s *Session) listFolders() ([]tutaFolder, error) {
	var out []tutaFolder
	for _, groupID := range s.mailGroups {
		root, err := s.rest.getEntity("tutanota", "mailboxgrouproot", modelVersionTutanota, groupID)
		if err != nil {
			return nil, err
		}
		mailboxID := root.assocID("699")
		if mailboxID == "" {
			continue
		}
		mb, err := s.rest.getEntity("tutanota", "mailbox", modelVersionTutanota, mailboxID)
		if err != nil {
			return nil, err
		}
		ref := mb.agg("443")
		if ref == nil {
			continue
		}
		setsList := ref.assocID("442")
		if setsList == "" {
			continue
		}
		sets, err := s.rest.loadRange("tutanota", "mailset", modelVersionTutanota, setsList, "------------", 1000, false)
		if err != nil {
			return nil, err
		}
		for _, set := range sets {
			if _, err := s.decryptInstance(set, mailSetSpec); err != nil {
				return nil, err
			}
			kind := set.str("436")
			if kind == "7" {
				continue
			}
			f := tutaFolder{
				kind:      kind,
				groupID:   groupID,
				setListID: setsList,
				entriesID: set.assocID("1459"),
			}
			f.setID = set.elemID("431")
			if sys, ok := systemFolderNames[kind]; ok {
				f.name = sys.name
				f.special = sys.special
			} else {
				f.name = set.str("435")
				if f.name == "" {
					f.name = f.setID
				}
			}
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Session) folderByName(name string) (tutaFolder, error) {
	if s.folders == nil {
		folders, err := s.listFolders()
		if err != nil {
			return tutaFolder{}, err
		}
		s.folders = folders
	}
	for _, f := range s.folders {
		if f.name == name {
			return f, nil
		}
	}
	return tutaFolder{}, fmt.Errorf("tuta: folder %q not found", name)
}

func (s *Session) folderBySpecial(special string) (tutaFolder, error) {
	if s.folders == nil {
		folders, err := s.listFolders()
		if err != nil {
			return tutaFolder{}, err
		}
		s.folders = folders
	}
	for _, f := range s.folders {
		if f.special == special {
			return f, nil
		}
	}
	return tutaFolder{}, fmt.Errorf("tuta: no %s folder", special)
}

type mailMeta struct {
	id         string
	listID     string
	subject    string
	from       mail.Addr
	firstRcpt  mail.Addr
	to         []mail.Addr
	cc         []mail.Addr
	date       time.Time
	unread     bool
	pinned     bool
	confid     bool
	messageID  string
	references []string
	inReplyTo  string
	attachIDs  [][2]string
	detailsArc string
	detailsID  string
	hasDetails bool
}

func addrFromInstance(in instance) mail.Addr {
	if in == nil {
		return mail.Addr{}
	}
	return mail.Addr{Name: in.str("94"), Email: in.str("95")}
}

func addrsFromAggList(inst instance, key string) []mail.Addr {
	items := inst.aggList(key)
	out := make([]mail.Addr, 0, len(items))
	for _, it := range items {
		out = append(out, addrFromInstance(it))
	}
	return out
}

func (m *mailMeta) toMsg(uid uint32, folderKind string) mail.Msg {
	msg := mail.Msg{
		UID:       uid,
		MessageID: m.messageID,
		InReplyTo: m.inReplyTo,
		Subject:   m.subject,
		Date:      m.date,
		HasAttach: len(m.attachIDs) > 0,
	}
	if msg.MessageID == "" {
		msg.MessageID = m.id + "@tuta"
	}
	msg.From = []mail.Addr{m.from}
	msg.To = m.to
	if len(msg.To) == 0 && m.firstRcpt.Email != "" {
		msg.To = []mail.Addr{m.firstRcpt}
	}
	msg.Cc = m.cc
	if !m.unread {
		msg.Flags |= mail.FlagSeen
	}
	if m.pinned {
		msg.Flags |= mail.FlagFlagged
	}
	if folderKind == "6" {
		msg.Flags |= mail.FlagDraft
	}
	return msg
}

func parseHeadersText(text string) (messageID string, references []string, inReplyTo string) {
	var headers []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && len(headers) > 0 {
			headers[len(headers)-1] += " " + strings.TrimSpace(line)
			continue
		}
		headers = append(headers, line)
	}
	for _, line := range headers {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "message-id":
			if ids := extractMsgIDs(value); len(ids) > 0 {
				messageID = ids[0]
			}
		case "references":
			references = extractMsgIDs(value)
		case "in-reply-to":
			if ids := extractMsgIDs(value); len(ids) > 0 {
				inReplyTo = ids[0]
			}
		}
	}
	return messageID, references, inReplyTo
}

func extractMsgIDs(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "<")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], ">")
		if j < 0 {
			return out
		}
		id := s[i+1 : i+j]
		if id != "" {
			out = append(out, id)
		}
		s = s[i+j+1:]
	}
}
