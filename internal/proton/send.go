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
// The draft creation and send flows in this file are adapted from hydroxide
// (github.com/emersion/hydroxide, imap/message.go and smtp/smtp.go),
// MIT-licensed as above.

package proton

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/hydroxide/protonmail"
)

func formatHeader(h mail.Header) string {
	var b bytes.Buffer
	fields := h.Fields()
	for fields.Next() {
		b.WriteString(fmt.Sprintf("%s: %s\r\n", fields.Key(), fields.Value()))
	}
	return b.String()
}

func protonmailAddressList(addresses []*mail.Address) []*protonmail.MessageAddress {
	l := make([]*protonmail.MessageAddress, len(addresses))
	for i, addr := range addresses {
		l[i] = &protonmail.MessageAddress{
			Name:    addr.Name,
			Address: addr.Address,
		}
	}
	return l
}

func (s *Session) createMessageLocked(r io.Reader) (msg *protonmail.Message, attachmentKeys map[string]*packet.EncryptedKey, body []byte, bodyType string, privateKey *openpgp.Entity, err error) {
	mr, err := mail.CreateReader(r)
	if err != nil {
		return nil, nil, nil, "", nil, err
	}

	subject, _ := mr.Header.Subject()
	fromList, _ := mr.Header.AddressList("From")
	toList, _ := mr.Header.AddressList("To")
	ccList, _ := mr.Header.AddressList("Cc")
	bccList, _ := mr.Header.AddressList("Bcc")

	if len(fromList) != 1 {
		return nil, nil, nil, "", nil, errors.New("the From field must contain exactly one address")
	}
	if len(toList) == 0 && len(ccList) == 0 && len(bccList) == 0 {
		return nil, nil, nil, "", nil, errors.New("no recipient specified")
	}

	addrs, err := s.addressesLocked()
	if err != nil {
		return nil, nil, nil, "", nil, err
	}
	fromAddrStr := fromList[0].Address
	var fromAddr *protonmail.Address
	for _, addr := range addrs {
		if strings.EqualFold(addr.Email, fromAddrStr) {
			fromAddr = addr
			break
		}
	}
	if fromAddr == nil {
		return nil, nil, nil, "", nil, errors.New("unknown sender address")
	}
	if len(fromAddr.Keys) == 0 {
		return nil, nil, nil, "", nil, errors.New("sender address has no private key")
	}

	encryptedPrivateKey, err := fromAddr.Keys[0].Entity()
	if err != nil {
		return nil, nil, nil, "", nil, fmt.Errorf("cannot parse sender private key: %v", err)
	}

	for _, e := range s.keys {
		if e.PrimaryKey.KeyId == encryptedPrivateKey.PrimaryKey.KeyId {
			privateKey = e
			break
		}
	}
	if privateKey == nil {
		return nil, nil, nil, "", nil, errors.New("sender address key hasn't been decrypted")
	}

	msg = &protonmail.Message{
		ToList:    protonmailAddressList(toList),
		CCList:    protonmailAddressList(ccList),
		BCCList:   protonmailAddressList(bccList),
		Subject:   subject,
		Header:    formatHeader(mr.Header),
		AddressID: fromAddr.ID,
	}

	plaintext, err := msg.Encrypt([]*openpgp.Entity{privateKey}, privateKey)
	if err != nil {
		return nil, nil, nil, "", nil, err
	}
	if err := plaintext.Close(); err != nil {
		return nil, nil, nil, "", nil, err
	}

	msg, err = s.c.CreateDraftMessage(msg, "")
	if err != nil {
		return nil, nil, nil, "", nil, fmt.Errorf("cannot create draft message: %v", err)
	}
	draftDone := false
	defer func() {
		if !draftDone {
			_ = s.c.DeleteMessages([]string{msg.ID})
		}
	}()

	attachmentKeys = make(map[string]*packet.EncryptedKey)
	var bodyBuf *bytes.Buffer

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, nil, "", nil, err
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			t, _, err := h.ContentType()
			if err != nil {
				continue
			}
			if bodyBuf != nil && t != "text/html" {
				continue
			}
			bodyBuf = &bytes.Buffer{}
			bodyType = t
			if _, err := io.Copy(bodyBuf, p.Body); err != nil {
				return nil, nil, nil, "", nil, err
			}
		case *mail.AttachmentHeader:
			t, _, err := h.ContentType()
			if err != nil {
				continue
			}
			filename, err := h.Filename()
			if err != nil {
				continue
			}

			att := &protonmail.Attachment{
				MessageID: msg.ID,
				Name:      filename,
				MIMEType:  t,
				ContentID: h.Get("Content-Id"),
			}

			attKey, err := att.GenerateKey([]*openpgp.Entity{privateKey})
			if err != nil {
				return nil, nil, nil, "", nil, fmt.Errorf("cannot generate attachment key: %v", err)
			}

			pr, pw := io.Pipe()
			go func() {
				cleartext, err := att.Encrypt(pw, privateKey)
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				if _, err := io.Copy(cleartext, p.Body); err != nil {
					pw.CloseWithError(err)
					return
				}
				pw.CloseWithError(cleartext.Close())
			}()

			att, err = s.c.CreateAttachment(att, pr)
			if err != nil {
				return nil, nil, nil, "", nil, fmt.Errorf("cannot upload attachment: %v", err)
			}

			msg.Attachments = append(msg.Attachments, att)
			attachmentKeys[att.ID] = attKey
		}
	}

	if bodyBuf == nil {
		return nil, nil, nil, "", nil, errors.New("message doesn't contain a body part")
	}
	body = bodyBuf.Bytes()

	msg.MIMEType = bodyType
	plaintext, err = msg.Encrypt([]*openpgp.Entity{privateKey}, privateKey)
	if err != nil {
		return nil, nil, nil, "", nil, err
	}
	if _, err := io.Copy(plaintext, bytes.NewReader(body)); err != nil {
		return nil, nil, nil, "", nil, err
	}
	if err := plaintext.Close(); err != nil {
		return nil, nil, nil, "", nil, err
	}

	if _, err := s.c.UpdateDraftMessage(msg); err != nil {
		return nil, nil, nil, "", nil, fmt.Errorf("cannot update draft message: %v", err)
	}

	draftDone = true
	return msg, attachmentKeys, body, bodyType, privateKey, nil
}

func (s *Session) SendRaw(raw []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg, attachmentKeys, body, bodyType, privateKey, err := s.createMessageLocked(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	sentDone := false
	defer func() {
		if !sentDone {
			_ = s.c.DeleteMessages([]string{msg.ID})
		}
	}()

	recipients := make([]*protonmail.MessageAddress, 0, len(msg.ToList)+len(msg.CCList)+len(msg.BCCList))
	recipients = append(recipients, msg.ToList...)
	recipients = append(recipients, msg.CCList...)
	recipients = append(recipients, msg.BCCList...)

	var plaintextRecipients []string
	encryptedRecipients := make(map[string]*openpgp.Entity)
	for _, rcpt := range recipients {
		resp, err := s.c.GetPublicKeys(rcpt.Address)
		if err != nil {
			return "", fmt.Errorf("cannot get public key for address %q: %v", rcpt.Address, err)
		}
		if len(resp.Keys) == 0 {
			plaintextRecipients = append(plaintextRecipients, rcpt.Address)
			continue
		}
		pub, err := resp.Keys[0].Entity()
		if err != nil {
			return "", err
		}
		encryptedRecipients[rcpt.Address] = pub
	}

	outgoing := &protonmail.OutgoingMessage{ID: msg.ID}

	if len(plaintextRecipients) > 0 {
		plaintextSet := protonmail.NewMessagePackageSet(attachmentKeys)

		plaintext, err := plaintextSet.Encrypt(bodyType, privateKey)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(plaintext, bytes.NewReader(body)); err != nil {
			plaintext.Close()
			return "", err
		}
		if err := plaintext.Close(); err != nil {
			return "", err
		}

		for _, rcpt := range plaintextRecipients {
			pkg, err := plaintextSet.AddCleartext(rcpt)
			if err != nil {
				return "", err
			}
			pkg.Signature = 0
		}

		outgoing.Packages = append(outgoing.Packages, plaintextSet)
	}

	if len(encryptedRecipients) > 0 {
		encryptedSet := protonmail.NewMessagePackageSet(attachmentKeys)

		plaintext, err := encryptedSet.Encrypt(bodyType, privateKey)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(plaintext, bytes.NewReader(body)); err != nil {
			plaintext.Close()
			return "", err
		}
		if err := plaintext.Close(); err != nil {
			return "", err
		}

		for rcpt, pub := range encryptedRecipients {
			if _, err := encryptedSet.AddInternal(rcpt, pub); err != nil {
				return "", err
			}
		}

		outgoing.Packages = append(outgoing.Packages, encryptedSet)
	}

	sent, _, err := s.c.SendMessage(outgoing)
	if err != nil {
		return "", fmt.Errorf("cannot send message: %v", err)
	}
	sentDone = true
	s.cur = nil
	return sent.ID, nil
}
