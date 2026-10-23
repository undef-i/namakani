package mail

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/textproto"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	FlagSeen uint32 = 1 << iota
	FlagFlagged
	FlagAnswered
	FlagDeleted
	FlagDraft
)

type Account struct {
	ID            int64
	Kind          string
	Label         string
	FromName      string
	Email         string
	ImapHost      string
	ImapPort      int
	ImapTLS       string
	SMTPHost      string
	SMTPPort      int
	SMTPTLS       string
	Username      string
	Password      string
	TLSSkipVerify bool
}

type Addr struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type FolderInfo struct {
	Name       string
	SpecialUse string
}

type Msg struct {
	UID       uint32
	MessageID string
	InReplyTo string
	Subject   string
	From      []Addr
	To        []Addr
	Cc        []Addr
	Date      time.Time
	Flags     uint32
	Size      int64
	HasAttach bool
	ModSeq    uint64
}

type Client struct {
	c    *imapclient.Client
	Caps imap.CapSet
}

func TLSConfigFor(host string, skipVerify bool) *tls.Config {
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if skipVerify {
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

func Connect(a Account, handler *imapclient.UnilateralDataHandler) (*Client, error) {
	addr := net.JoinHostPort(a.ImapHost, fmt.Sprint(a.ImapPort))
	opts := &imapclient.Options{
		TLSConfig:             TLSConfigFor(a.ImapHost, a.TLSSkipVerify),
		UnilateralDataHandler: handler,
	}
	var (
		c   *imapclient.Client
		err error
	)
	switch a.ImapTLS {
	case "tls":
		c, err = imapclient.DialTLS(addr, opts)
	case "starttls":
		c, err = imapclient.DialStartTLS(addr, opts)
	default:
		c, err = imapclient.DialInsecure(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	if err := c.Login(a.Username, a.Password).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("login: %w", err)
	}
	caps, err := c.Capability().Wait()
	if err != nil {
		caps = c.Caps()
	}
	return &Client{c: c, Caps: caps}, nil
}

func (cl *Client) Close() {
	_ = cl.c.Logout().Wait()
	_ = cl.c.Close()
}

func (cl *Client) HasCap(cap imap.Cap) bool { return cl.Caps.Has(cap) }

var attrSpecialUse = map[string]string{
	"\\Inbox":   "inbox",
	"\\Sent":    "sent",
	"\\Drafts":  "drafts",
	"\\Trash":   "trash",
	"\\Junk":    "junk",
	"\\Archive": "archive",
	"\\Flagged": "flagged",
	"\\All":     "all",
}

func specialUseFromName(name string) string {
	seg := name
	if i := strings.LastIndexAny(seg, "/."); i >= 0 {
		seg = seg[i+1:]
	}
	switch strings.ToLower(seg) {
	case "inbox":
		return "inbox"
	case "sent", "sent mail", "sent items":
		return "sent"
	case "drafts", "draft":
		return "drafts"
	case "trash", "deleted", "deleted items", "bin":
		return "trash"
	case "junk", "spam", "bulk", "junk e-mail":
		return "junk"
	case "archive", "archives", "all mail":
		return "archive"
	}
	return ""
}

func (cl *Client) ListFolders() ([]FolderInfo, error) {
	datas, err := cl.c.List("", "*", &imap.ListOptions{}).Collect()
	if err != nil {
		return nil, err
	}
	var out []FolderInfo
	for _, d := range datas {
		noselect := false
		special := ""
		for _, attr := range d.Attrs {
			if attr == "\\Noselect" {
				noselect = true
			}
			if su, ok := attrSpecialUse[string(attr)]; ok {
				special = su
			}
		}
		if noselect {
			continue
		}
		if special == "" {
			special = specialUseFromName(d.Mailbox)
		}
		out = append(out, FolderInfo{Name: d.Mailbox, SpecialUse: special})
	}
	return out, nil
}

func (cl *Client) Select(name string) (*SelectData, error) {
	d, err := cl.c.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, err
	}
	return &SelectData{UIDValidity: d.UIDValidity, UIDNext: uint32(d.UIDNext), NumMessages: d.NumMessages}, nil
}

func (cl *Client) SelectRW(name string) (*SelectData, error) {
	d, err := cl.c.Select(name, &imap.SelectOptions{ReadOnly: false}).Wait()
	if err != nil {
		return nil, err
	}
	return &SelectData{UIDValidity: d.UIDValidity, UIDNext: uint32(d.UIDNext), NumMessages: d.NumMessages}, nil
}

func (cl *Client) Capabilities() []string {
	out := make([]string, 0, len(cl.Caps))
	for cap := range cl.Caps {
		out = append(out, string(cap))
	}
	sort.Strings(out)
	return out
}

func (cl *Client) FetchRaw(uid uint32) ([]byte, error) {
	set := imap.UIDSet{}
	set.AddNum(imap.UID(uid))
	opts := &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}
	cmd := cl.c.Fetch(set, opts)
	var raw []byte
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		buf, err := data.Collect()
		if err != nil {
			cmd.Close()
			return nil, err
		}
		for _, sec := range buf.BodySection {
			raw = append(raw, sec.Bytes...)
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("message %d not found on server", uid)
	}
	return raw, nil
}

type ThreadHeaders struct {
	References []string
	InReplyTo  string
}

var msgIDTokenRe = regexp.MustCompile(`<([^<>\s]+)>`)

func (cl *Client) FetchThreadHeaders(uids []uint32) (map[uint32]ThreadHeaders, error) {
	out := make(map[uint32]ThreadHeaders, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	set := imap.UIDSet{}
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	section := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"References", "In-Reply-To"},
		Peek:         true,
	}
	opts := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}
	cmd := cl.c.Fetch(set, opts)
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		buf, err := data.Collect()
		if err != nil {
			cmd.Close()
			return nil, err
		}
		raw := buf.FindBodySection(section)
		if raw == nil {
			continue
		}
		hdr, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
		if err != nil {
			continue
		}
		var th ThreadHeaders
		for _, m := range msgIDTokenRe.FindAllStringSubmatch(hdr.Get("References"), -1) {
			th.References = append(th.References, m[1])
		}
		if m := msgIDTokenRe.FindStringSubmatch(hdr.Get("In-Reply-To")); m != nil {
			th.InReplyTo = m[1]
		}
		out[uint32(buf.UID)] = th
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (cl *Client) StoreFlag(uid uint32, flag imap.Flag, add bool) error {
	return cl.StoreFlags([]uint32{uid}, flag, add)
}

func (cl *Client) StoreFlags(uids []uint32, flag imap.Flag, add bool) error {
	if len(uids) == 0 {
		return nil
	}
	set := imap.UIDSet{}
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}
	op := imap.StoreFlagsAdd
	if !add {
		op = imap.StoreFlagsDel
	}
	_, err := cl.c.Store(set, &imap.StoreFlags{Op: op, Flags: []imap.Flag{flag}}, nil).Collect()
	return err
}

func firstDestUID(set imap.NumSet) uint32 {
	uids, ok := set.(imap.UIDSet)
	if !ok {
		return 0
	}
	nums, ok := uids.Nums()
	if ok && len(nums) > 0 {
		return uint32(nums[0])
	}
	return 0
}

func (cl *Client) MoveMessage(uid uint32, dest string) (uint32, error) {
	set := imap.UIDSet{}
	set.AddNum(imap.UID(uid))
	if cl.HasCap(imap.CapMove) {
		data, err := cl.c.Move(set, dest).Wait()
		if err != nil {
			return 0, err
		}
		return firstDestUID(data.DestUIDs), nil
	}
	data, err := cl.c.Copy(set, dest).Wait()
	if err != nil {
		return 0, err
	}
	destUID := firstDestUID(data.DestUIDs)
	if err := cl.DeleteMessage(uid); err != nil {
		return 0, err
	}
	return destUID, nil
}

func (cl *Client) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	cmd := cl.c.Append(folder, int64(len(raw)), &imap.AppendOptions{Flags: flags})
	if _, err := cmd.Write(raw); err != nil {
		return 0, err
	}
	if err := cmd.Close(); err != nil {
		return 0, err
	}
	data, err := cmd.Wait()
	if err != nil {
		return 0, err
	}
	return uint32(data.UID), nil
}

func (cl *Client) DeleteMessage(uid uint32) error {
	if err := cl.StoreFlag(uid, imap.FlagDeleted, true); err != nil {
		return err
	}
	_, err := cl.c.Expunge().Collect()
	return err
}

func (cl *Client) Idle() (*imapclient.IdleCommand, error) {
	return cl.c.Idle()
}

func flagMask(flags []imap.Flag) uint32 {
	var mask uint32
	for _, f := range flags {
		switch f {
		case imap.FlagSeen:
			mask |= FlagSeen
		case imap.FlagFlagged:
			mask |= FlagFlagged
		case imap.FlagAnswered:
			mask |= FlagAnswered
		case imap.FlagDeleted:
			mask |= FlagDeleted
		case imap.FlagDraft:
			mask |= FlagDraft
		}
	}
	return mask
}

func convertAddrs(in []imap.Address) []Addr {
	out := make([]Addr, 0, len(in))
	for _, a := range in {
		email := a.Mailbox
		if a.Host != "" {
			email += "@" + a.Host
		}
		out = append(out, Addr{Name: a.Name, Email: email})
	}
	return out
}

func hasAttachment(bs imap.BodyStructure) bool {
	found := false
	if bs == nil {
		return false
	}
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		if sp.Extended != nil && sp.Extended.Disposition != nil {
			d := sp.Extended.Disposition
			if strings.EqualFold(d.Value, "attachment") || d.Params["filename"] != "" {
				found = true
				return true
			}
			if strings.EqualFold(d.Value, "inline") {
				return true
			}
		}
		mt := sp.MediaType()
		if mt == "text/plain" || mt == "text/html" || mt == "message/rfc822" {
			return true
		}
		if sp.Params["name"] != "" {
			found = true
		}
		return true
	})
	return found
}

func convertMsg(buf *imapclient.FetchMessageBuffer) Msg {
	m := Msg{
		UID:    uint32(buf.UID),
		Flags:  flagMask(buf.Flags),
		Size:   buf.RFC822Size,
		ModSeq: buf.ModSeq,
	}
	m.Date = buf.InternalDate
	if buf.Envelope != nil {
		m.Subject = buf.Envelope.Subject
		m.MessageID = buf.Envelope.MessageID
		if n := len(buf.Envelope.InReplyTo); n > 0 {
			m.InReplyTo = buf.Envelope.InReplyTo[n-1]
		}
		m.From = convertAddrs(buf.Envelope.From)
		m.To = convertAddrs(buf.Envelope.To)
		m.Cc = convertAddrs(buf.Envelope.Cc)
		if !buf.Envelope.Date.IsZero() {
			m.Date = buf.Envelope.Date
		}
	}
	m.HasAttach = hasAttachment(buf.BodyStructure)
	return m
}

func (cl *Client) fetch(uids imap.UIDSet, opts *imap.FetchOptions) ([]Msg, error) {
	cmd := cl.c.Fetch(uids, opts)
	var out []Msg
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		buf, err := data.Collect()
		if err != nil {
			cmd.Close()
			return nil, err
		}
		out = append(out, convertMsg(buf))
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (cl *Client) FetchEnvelopes(uids []uint32) ([]Msg, error) {
	set := imap.UIDSet{}
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	opts := &imap.FetchOptions{
		UID:           true,
		Envelope:      true,
		Flags:         true,
		InternalDate:  true,
		RFC822Size:    true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}
	if cl.HasCap(imap.CapCondStore) {
		opts.ModSeq = true
	}
	return cl.fetch(set, opts)
}

func (cl *Client) FetchFlags(changedSince uint64) ([]Msg, error) {
	set := imap.UIDSet{}
	set.AddRange(1, 0)
	opts := &imap.FetchOptions{UID: true, Flags: true}
	if cl.HasCap(imap.CapCondStore) {
		opts.ModSeq = true
		if changedSince > 0 {
			opts.ChangedSince = changedSince
		}
	}
	return cl.fetch(set, opts)
}
