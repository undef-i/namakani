package tuta

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	namakani "namakani/internal/mail"
)

func (s *Session) FetchRaw(uid uint32) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.currentLocked()
	if err != nil {
		return nil, err
	}
	id, ok := fc.idByUID[uid]
	if !ok {
		return nil, fmt.Errorf("tuta: message %d not found in %s", uid, fc.name)
	}
	return s.buildRaw(fc.meta[id])
}

type attachmentData struct {
	name string
	mime string
	cid  string
	data []byte
}

func (s *Session) loadAttachment(tuple [2]string) (attachmentData, error) {
	f, err := s.rest.getEntity("tutanota", "file", modelVersionTutanota, tuple[0], tuple[1])
	if err != nil {
		return attachmentData{}, err
	}
	sk, err := s.decryptInstance(f, fileSpec)
	if err != nil {
		return attachmentData{}, err
	}
	if sk == nil {
		sk, err = s.groupKey(f.str("580"), parseKeyVersion(f.str("1391")))
		if err != nil {
			return attachmentData{}, err
		}
	}
	out := attachmentData{
		name: f.str("21"),
		mime: f.str("23"),
		cid:  f.str("924"),
	}
	if out.mime == "" {
		out.mime = "application/octet-stream"
	}
	var buf bytes.Buffer
	for _, blob := range f.aggList("1225") {
		archiveID := blob.str("1884")
		blobID := blob.str("1906")
		chunks, err := s.downloadBlobs(archiveID, []string{blobID})
		if err != nil {
			return attachmentData{}, err
		}
		enc, ok := chunks[blobID]
		if !ok {
			return attachmentData{}, fmt.Errorf("tuta: blob %s not returned", blobID)
		}
		plain, err := decryptBytes(sk, enc)
		if err != nil {
			return attachmentData{}, err
		}
		buf.Write(plain)
	}
	out.data = buf.Bytes()
	return out, nil
}

func looksHTML(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "<") {
		return false
	}
	lower := strings.ToLower(t)
	for _, tag := range []string{"<html", "<body", "<div", "<p>", "<p ", "<br", "<table", "<span", "<h1", "<h2", "<ul", "<ol", "<a "} {
		if strings.Contains(lower, tag) {
			return true
		}
	}
	return false
}

func goMailAddr(a namakani.Addr) *mail.Address {
	return &mail.Address{Name: a.Name, Address: a.Email}
}

func goMailAddrs(in []namakani.Addr) []*mail.Address {
	out := make([]*mail.Address, 0, len(in))
	for _, a := range in {
		out = append(out, goMailAddr(a))
	}
	return out
}

func (s *Session) buildRaw(m *mailMeta) ([]byte, error) {
	details, err := s.loadDetails(m)
	if err != nil {
		return nil, err
	}
	var bodyText string
	var toList, ccList, bccList []namakani.Addr
	var replyTos []namakani.Addr
	var sentDate time.Time
	var headersText string
	if details != nil {
		if rcpts := details.agg("1286"); rcpts != nil {
			toList = addrsFromAggList(rcpts, "1279")
			ccList = addrsFromAggList(rcpts, "1280")
			bccList = addrsFromAggList(rcpts, "1281")
		}
		for _, rt := range details.aggList("1285") {
			replyTos = append(replyTos, namakani.Addr{Name: rt.str("614"), Email: rt.str("615")})
		}
		if ms := details.num("1284"); ms > 0 {
			sentDate = time.UnixMilli(ms)
		}
		if body := details.agg("1288"); body != nil {
			bodyText = body.str("1275")
			if bodyText == "" {
				bodyText = body.str("1276")
			}
		}
		if hdr := details.agg("1287"); hdr != nil {
			headersText = hdr.str("1271")
			if headersText == "" {
				headersText = hdr.str("1272")
			}
		}
	}
	if len(toList) == 0 {
		toList = m.to
	}
	if len(ccList) == 0 {
		ccList = m.cc
	}
	date := m.date
	if !sentDate.IsZero() {
		date = sentDate
	}
	messageID := m.messageID
	references := m.references
	inReplyTo := m.inReplyTo
	if headersText != "" {
		mid, refs, irt := parseHeadersText(headersText)
		if mid != "" {
			messageID = mid
		}
		if len(refs) > 0 {
			references = refs
		}
		if irt != "" {
			inReplyTo = irt
		}
	}

	sum := sha1.Sum([]byte(m.id))
	var h mail.Header
	h.SetContentType("multipart/mixed", map[string]string{"boundary": hex.EncodeToString(sum[:])})
	h.SetDate(date)
	h.SetSubject(m.subject)
	h.SetAddressList("From", []*mail.Address{goMailAddr(m.from)})
	if len(replyTos) > 0 {
		h.SetAddressList("Reply-To", goMailAddrs(replyTos))
	}
	if len(toList) > 0 {
		h.SetAddressList("To", goMailAddrs(toList))
	}
	if len(ccList) > 0 {
		h.SetAddressList("Cc", goMailAddrs(ccList))
	}
	if len(bccList) > 0 {
		h.SetAddressList("Bcc", goMailAddrs(bccList))
	}
	if len(references) > 0 {
		refs := make([]string, len(references))
		for i, r := range references {
			refs[i] = "<" + r + ">"
		}
		h.Set("References", strings.Join(refs, " "))
	}
	if inReplyTo != "" {
		h.Set("In-Reply-To", "<"+inReplyTo+">")
	}
	h.Set("Message-Id", "<"+messageID+">")

	b := new(bytes.Buffer)
	w, err := message.CreateWriter(b, h.Header)
	if err != nil {
		return nil, err
	}
	var ih mail.InlineHeader
	mimeType := "text/plain"
	if looksHTML(bodyText) {
		mimeType = "text/html"
	}
	ih.SetContentType(mimeType, map[string]string{"charset": "utf-8"})
	ih.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, err := w.CreatePart(ih.Header)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(pw, strings.NewReader(bodyText)); err != nil {
		return nil, err
	}
	if err := pw.Close(); err != nil {
		return nil, err
	}
	for _, tuple := range m.attachIDs {
		att, err := s.loadAttachment(tuple)
		if err != nil {
			return nil, err
		}
		var ah mail.AttachmentHeader
		ah.SetContentType(att.mime, nil)
		ah.Set("Content-Transfer-Encoding", "base64")
		ah.SetFilename(att.name)
		if att.cid != "" {
			ah.Set("Content-Id", att.cid)
		}
		apw, err := w.CreatePart(ah.Header)
		if err != nil {
			return nil, err
		}
		if _, err := apw.Write(att.data); err != nil {
			return nil, err
		}
		if err := apw.Close(); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
