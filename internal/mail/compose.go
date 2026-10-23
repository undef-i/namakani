package mail

import (
	"bytes"
	"time"

	"github.com/emersion/go-message/mail"
)

type OutgoingAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

type ComposeInput struct {
	From        Addr
	To          []Addr
	Cc          []Addr
	Subject     string
	Text        string
	HTML        string
	Date        time.Time
	MessageID   string
	InReplyTo   string
	References  string
	Attachments []OutgoingAttachment
}

func toMailAddresses(addrs []Addr) []*mail.Address {
	out := make([]*mail.Address, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, &mail.Address{Name: a.Name, Address: a.Email})
	}
	return out
}

func BuildMessage(in ComposeInput) ([]byte, error) {
	var buf bytes.Buffer
	h := mail.Header{}
	h.SetAddressList("From", []*mail.Address{{Name: in.From.Name, Address: in.From.Email}})
	if len(in.To) > 0 {
		h.SetAddressList("To", toMailAddresses(in.To))
	}
	if len(in.Cc) > 0 {
		h.SetAddressList("Cc", toMailAddresses(in.Cc))
	}
	h.SetSubject(in.Subject)
	h.SetDate(in.Date)
	if in.MessageID != "" {
		h.SetMessageID(in.MessageID)
	}
	if in.InReplyTo != "" {
		h.Set("In-Reply-To", in.InReplyTo)
	}
	if in.References != "" {
		h.Set("References", in.References)
	}
	w, err := mail.CreateWriter(&buf, h)
	if err != nil {
		return nil, err
	}
	if in.HTML != "" {
		iw, err := w.CreateInline()
		if err != nil {
			return nil, err
		}
		var th mail.InlineHeader
		th.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
		tp, err := iw.CreatePart(th)
		if err != nil {
			return nil, err
		}
		if _, err := tp.Write([]byte(in.Text)); err != nil {
			return nil, err
		}
		if err := tp.Close(); err != nil {
			return nil, err
		}
		var hh mail.InlineHeader
		hh.SetContentType("text/html", map[string]string{"charset": "utf-8"})
		hp, err := iw.CreatePart(hh)
		if err != nil {
			return nil, err
		}
		if _, err := hp.Write([]byte(in.HTML)); err != nil {
			return nil, err
		}
		if err := hp.Close(); err != nil {
			return nil, err
		}
		if err := iw.Close(); err != nil {
			return nil, err
		}
	} else {
		var ih mail.InlineHeader
		ih.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
		part, err := w.CreateSingleInline(ih)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write([]byte(in.Text)); err != nil {
			return nil, err
		}
		if err := part.Close(); err != nil {
			return nil, err
		}
	}
	for _, att := range in.Attachments {
		var ah mail.AttachmentHeader
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		ah.SetContentType(ct, nil)
		ah.SetFilename(att.Filename)
		aw, err := w.CreateAttachment(ah)
		if err != nil {
			return nil, err
		}
		if _, err := aw.Write(att.Data); err != nil {
			return nil, err
		}
		if err := aw.Close(); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
