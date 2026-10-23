// Copyright (c) 2017 emersion
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software (the "Software"), to deal in the Software without
// restriction, including without limitation the rights to use, copy, modify,
// merge, publish, distribute, sublicense, and/or sell copies of the Software,
// and to permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//
// The MIME reconstruction in this file is adapted from hydroxide
// (github.com/emersion/hydroxide, imap/message.go), MIT-licensed as above.

package proton

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/hydroxide/protonmail"

	namakani "namakani/internal/mail"
)

func msgBoundary(msg *protonmail.Message) string {
	h := sha1.Sum([]byte(msg.ID))
	return hex.EncodeToString(h[:])
}

func splitMIMEType(t string) (string, string) {
	parts := strings.SplitN(t, "/", 2)
	if len(parts) < 2 {
		return "text", "plain"
	}
	return parts[0], parts[1]
}

func inlineHeader(msg *protonmail.Message) message.Header {
	var h mail.InlineHeader
	mt := msg.MIMEType
	if mt == "" {
		mt = "text/plain"
	}
	params := map[string]string{"charset": "utf-8"}
	if hdr, err := textproto.NewReader(bufio.NewReader(strings.NewReader(msg.Header + "\r\n\r\n"))).ReadMIMEHeader(); err == nil {
		if t, p, err := mime.ParseMediaType(hdr.Get("Content-Type")); err == nil && t == mt {
			params = p
		}
	}
	h.SetContentType(mt, params)
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	return h.Header
}

func attachmentHeader(att *protonmail.Attachment) message.Header {
	var h mail.AttachmentHeader
	h.SetContentType(att.MIMEType, nil)
	h.Set("Content-Transfer-Encoding", "base64")
	h.SetFilename(att.Name)
	if att.ContentID != "" {
		h.Set("Content-Id", att.ContentID)
	}
	return h.Header
}

func mailAddress(addr *protonmail.MessageAddress) *mail.Address {
	if addr == nil {
		return &mail.Address{}
	}
	return &mail.Address{Name: addr.Name, Address: addr.Address}
}

func mailAddressList(addresses []*protonmail.MessageAddress) []*mail.Address {
	l := make([]*mail.Address, len(addresses))
	for i, addr := range addresses {
		l[i] = mailAddress(addr)
	}
	return l
}

var msgIDTokenRe = regexp.MustCompile(`<([^<>\s]+)>`)

func parseThreadHeaders(header string) namakani.ThreadHeaders {
	var th namakani.ThreadHeaders
	if strings.TrimSpace(header) == "" {
		return th
	}
	hdr, err := textproto.NewReader(bufio.NewReader(strings.NewReader(header + "\r\n\r\n"))).ReadMIMEHeader()
	if err != nil {
		return th
	}
	for _, m := range msgIDTokenRe.FindAllStringSubmatch(hdr.Get("References"), -1) {
		th.References = append(th.References, m[1])
	}
	if m := msgIDTokenRe.FindStringSubmatch(hdr.Get("In-Reply-To")); m != nil {
		th.InReplyTo = m[1]
	}
	return th
}

func rawMessageHeader(msg *protonmail.Message) message.Header {
	typeParams := map[string]string{"boundary": msgBoundary(msg)}

	var h mail.Header
	h.SetContentType("multipart/mixed", typeParams)
	h.SetDate(msg.Time.Time())
	h.SetSubject(msg.Subject)
	h.SetAddressList("From", []*mail.Address{mailAddress(msg.Sender)})
	if len(msg.ReplyTos) > 0 {
		h.SetAddressList("Reply-To", mailAddressList(msg.ReplyTos))
	}
	if len(msg.ToList) > 0 {
		h.SetAddressList("To", mailAddressList(msg.ToList))
	}
	if len(msg.CCList) > 0 {
		h.SetAddressList("Cc", mailAddressList(msg.CCList))
	}
	if len(msg.BCCList) > 0 {
		h.SetAddressList("Bcc", mailAddressList(msg.BCCList))
	}
	th := parseThreadHeaders(msg.Header)
	if len(th.References) > 0 {
		refs := make([]string, len(th.References))
		for i, r := range th.References {
			refs[i] = "<" + r + ">"
		}
		h.Set("References", strings.Join(refs, " "))
	}
	if th.InReplyTo != "" {
		h.Set("In-Reply-To", "<"+th.InReplyTo+">")
	}
	h.Set("Message-Id", fmt.Sprintf("<%s>", messageIDOf(msg)))
	return h.Header
}

func (s *Session) buildRawLocked(id string) ([]byte, error) {
	msg, err := s.c.GetMessage(id)
	if err != nil {
		return nil, err
	}
	b := new(bytes.Buffer)
	w, err := message.CreateWriter(b, rawMessageHeader(msg))
	if err != nil {
		return nil, err
	}
	pw, err := w.CreatePart(inlineHeader(msg))
	if err != nil {
		return nil, err
	}
	md, err := msg.Read(s.keys, nil)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(pw, md.UnverifiedBody); err != nil {
		return nil, err
	}
	if err := pw.Close(); err != nil {
		return nil, err
	}
	for _, att := range msg.Attachments {
		pw, err := w.CreatePart(attachmentHeader(att))
		if err != nil {
			return nil, err
		}
		rc, err := s.c.GetAttachment(att.ID)
		if err != nil {
			return nil, err
		}
		amd, err := att.Read(rc, s.keys, nil)
		if err != nil {
			rc.Close()
			return nil, err
		}
		_, copyErr := io.Copy(pw, amd.UnverifiedBody)
		rc.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if err := pw.Close(); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
