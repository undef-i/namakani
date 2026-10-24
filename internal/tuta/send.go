package tuta

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"

	"github.com/emersion/go-imap/v2"
	gomail "github.com/emersion/go-message/mail"

	"namakani/internal/mail"
)

type parsedMail struct {
	subject     string
	from        mail.Addr
	to          []mail.Addr
	cc          []mail.Addr
	bcc         []mail.Addr
	body        string
	bodyHTML    bool
	references  []string
	inReplyTo   string
	attachments []outAttachment
}

type outAttachment struct {
	name string
	mime string
	cid  string
	data []byte
}

func parseOutgoing(raw []byte) (*parsedMail, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	out := &parsedMail{}
	out.subject, _ = mr.Header.Subject()
	fromList, _ := mr.Header.AddressList("From")
	if len(fromList) != 1 {
		return nil, errors.New("tuta: the From field must contain exactly one address")
	}
	out.from = mail.Addr{Name: fromList[0].Name, Email: fromList[0].Address}
	toList, _ := mr.Header.AddressList("To")
	ccList, _ := mr.Header.AddressList("Cc")
	bccList, _ := mr.Header.AddressList("Bcc")
	conv := func(in []*gomail.Address) []mail.Addr {
		res := make([]mail.Addr, 0, len(in))
		for _, a := range in {
			res = append(res, mail.Addr{Name: a.Name, Email: a.Address})
		}
		return res
	}
	out.to = conv(toList)
	out.cc = conv(ccList)
	out.bcc = conv(bccList)
	if len(out.to)+len(out.cc)+len(out.bcc) == 0 {
		return nil, errors.New("tuta: no recipient specified")
	}
	refs := mr.Header.Get("References")
	out.references = extractMsgIDs(refs)
	if irt := mr.Header.Get("In-Reply-To"); irt != "" {
		if ids := extractMsgIDs(irt); len(ids) > 0 {
			out.inReplyTo = ids[0]
		}
	}
	var plainBody, htmlBody string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		switch h := p.Header.(type) {
		case *gomail.InlineHeader:
			t, _, err := h.ContentType()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(p.Body)
			if err != nil {
				return nil, err
			}
			switch t {
			case "text/html":
				htmlBody = string(data)
			case "text/plain":
				plainBody = string(data)
			}
		case *gomail.AttachmentHeader:
			t, _, err := h.ContentType()
			if err != nil {
				continue
			}
			filename, err := h.Filename()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(p.Body)
			if err != nil {
				return nil, err
			}
			out.attachments = append(out.attachments, outAttachment{
				name: filename,
				mime: t,
				cid:  strings.Trim(h.Get("Content-Id"), "<>"),
				data: data,
			})
		}
	}
	if htmlBody != "" {
		out.body = htmlBody
		out.bodyHTML = true
	} else {
		out.body = plainBody
	}
	return out, nil
}

func encField(key []byte, value string) string {
	return b64encode(encryptBytes(key, []byte(value)))
}

func draftRecipient(key []byte, a mail.Addr) map[string]any {
	return map[string]any{
		"484": encField(key, a.Name),
		"485": a.Email,
	}
}

func draftRecipients(key []byte, in []mail.Addr) []any {
	out := make([]any, 0, len(in))
	for _, a := range in {
		out = append(out, draftRecipient(key, a))
	}
	return out
}

func (s *Session) mailGroupKey() ([]byte, int, string, error) {
	if len(s.mailGroups) == 0 {
		return nil, 0, "", errors.New("tuta: no mail group")
	}
	groupID := s.mailGroups[0]
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	key, version, err := s.loadCurrentGroupKey(groupID)
	if err != nil {
		return nil, 0, "", err
	}
	s.cacheGroupKeyLocked(groupID, version, key)
	return key, version, groupID, nil
}

func (s *Session) createDraft(pm *parsedMail, confidential bool) (draftList, draftID string, sk []byte, err error) {
	groupKey, groupVersion, groupID, err := s.mailGroupKey()
	if err != nil {
		return "", "", nil, err
	}
	sk = make([]byte, 32)
	if _, err := rand.Read(sk); err != nil {
		return "", "", nil, err
	}
	ownerEnc := encryptKey(groupKey, sk)
	var added []any
	for _, att := range pm.attachments {
		da, err := s.uploadDraftAttachment(att, groupKey, groupVersion, groupID)
		if err != nil {
			return "", "", nil, err
		}
		added = append(added, da)
	}
	conversationType := "0"
	previousMessageID := ""
	if pm.inReplyTo != "" {
		conversationType = "1"
		previousMessageID = pm.inReplyTo
	}
	confVal := "0"
	if confidential {
		confVal = "1"
	}
	bodyCompressed := lz4Compress([]byte(pm.body))
	data := instance{
		"509":  "0",
		"510":  previousMessageID,
		"511":  conversationType,
		"512":  b64encode(ownerEnc),
		"1427": fmt.Sprint(groupVersion),
		"515": map[string]any{
			"498":  encField(sk, pm.subject),
			"499":  "",
			"500":  pm.from.Email,
			"501":  encField(sk, pm.from.Name),
			"502":  encField(sk, confVal),
			"1116": encField(sk, "0"),
			"1194": encFieldBytes(sk, bodyCompressed),
			"503":  draftRecipients(sk, pm.to),
			"504":  draftRecipients(sk, pm.cc),
			"505":  draftRecipients(sk, pm.bcc),
			"506":  added,
			"507":  []any{},
			"819":  []any{},
		},
	}
	ret, err := s.rest.callService(http.MethodPost, "tutanota", "draftservice", modelVersionTutanota, data)
	if err != nil {
		return "", "", nil, err
	}
	l, el, ok := ret.idTuple("518")
	if !ok {
		return "", "", nil, errors.New("tuta: draft service returned no draft id")
	}
	return l, el, sk, nil
}

func encFieldBytes(key, plain []byte) string {
	return b64encode(encryptBytes(key, plain))
}

func (s *Session) uploadDraftAttachment(att outAttachment, groupKey []byte, groupVersion int, groupID string) (map[string]any, error) {
	fileSK := make([]byte, 32)
	if _, err := rand.Read(fileSK); err != nil {
		return nil, err
	}
	var tokens []any
	data := att.data
	if len(data) == 0 {
		enc := encryptBytes(fileSK, nil)
		tok, err := s.uploadBlobChunk(groupID, enc)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, map[string]any{"1992": tok})
	}
	for start := 0; start < len(data); start += maxBlobSizeBytes {
		end := start + maxBlobSizeBytes
		if end > len(data) {
			end = len(data)
		}
		enc := encryptBytes(fileSK, data[start:end])
		tok, err := s.uploadBlobChunk(groupID, enc)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, map[string]any{"1992": tok})
	}
	ownerEnc := encryptKey(groupKey, fileSK)
	newFile := map[string]any{
		"488":  encFieldBytes(fileSK, []byte(att.name)),
		"489":  encFieldBytes(fileSK, []byte(att.mime)),
		"1226": tokens,
	}
	if att.cid != "" {
		newFile["925"] = encFieldBytes(fileSK, []byte(att.cid))
	} else {
		newFile["925"] = nil
	}
	return map[string]any{
		"493":  b64encode(ownerEnc),
		"1430": fmt.Sprint(groupVersion),
		"494":  newFile,
		"495":  []any{},
	}, nil
}

type recipientKey struct {
	address    string
	eccPub     []byte
	kyberPub   []byte
	rsaPub     []byte
	keyVersion int
	internal   bool
}

func (s *Session) lookupRecipient(email string) (recipientKey, error) {
	out, err := s.rest.callService(http.MethodGet, "sys", "publickeyservice", modelVersionSys, instance{
		"410":  "0",
		"411":  email,
		"2244": nil,
		"2468": "0",
	})
	if err != nil {
		return recipientKey{}, err
	}
	return recipientKey{
		address:    email,
		eccPub:     out.bytes("2148"),
		kyberPub:   out.bytes("2149"),
		rsaPub:     out.bytes("414"),
		keyVersion: parseKeyVersion(out.str("415")),
		internal:   true,
	}, nil
}

func parseRSAPublicKey(hexBytes []byte) (*rsa.PublicKey, error) {
	hexStr := string(hexBytes)
	pos := 0
	var parts [][]byte
	for pos < len(hexStr) {
		if pos+4 > len(hexStr) {
			return nil, errors.New("tuta: malformed rsa public key")
		}
		length, err := parseHexInt(hexStr[pos : pos+4])
		if err != nil {
			return nil, err
		}
		pos += 4
		if pos+length > len(hexStr) {
			return nil, errors.New("tuta: malformed rsa public key")
		}
		comp, err := hex.DecodeString(hexStr[pos : pos+length])
		if err != nil {
			return nil, err
		}
		parts = append(parts, comp)
		pos += length
	}
	if len(parts) == 0 {
		return nil, errors.New("tuta: empty rsa public key")
	}
	n := new(big.Int).SetBytes(parts[0])
	return &rsa.PublicKey{N: n, E: 65537}, nil
}

func parseRSAPrivateKey(raw []byte) (*rsa.PrivateKey, error) {
	if isHexString(raw) {
		return parseRSAPrivateKeyHex(string(raw))
	}
	return parseRSAPrivateKeyBinary(raw)
}

func isHexString(b []byte) bool {
	if len(b) == 0 || len(b)%2 != 0 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func rsaPrivateKeyFromParts(parts [][]byte) (*rsa.PrivateKey, error) {
	if len(parts) != 7 {
		return nil, fmt.Errorf("tuta: rsa private key has %d components, want 7", len(parts))
	}
	priv := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: new(big.Int).SetBytes(parts[0]), E: 65537},
		D:         new(big.Int).SetBytes(parts[1]),
		Primes: []*big.Int{
			new(big.Int).SetBytes(parts[2]),
			new(big.Int).SetBytes(parts[3]),
		},
		Precomputed: rsa.PrecomputedValues{
			Dp:   new(big.Int).SetBytes(parts[4]),
			Dq:   new(big.Int).SetBytes(parts[5]),
			Qinv: new(big.Int).SetBytes(parts[6]),
		},
	}
	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("tuta: invalid rsa private key: %w", err)
	}
	return priv, nil
}

func parseRSAPrivateKeyBinary(raw []byte) (*rsa.PrivateKey, error) {
	pos := 0
	var parts [][]byte
	for pos < len(raw) {
		if pos+2 > len(raw) {
			return nil, errors.New("tuta: malformed rsa private key")
		}
		hexLen := int(raw[pos])<<8 | int(raw[pos+1])
		pos += 2
		if hexLen%2 != 0 {
			return nil, errors.New("tuta: malformed rsa private key component length")
		}
		n := hexLen / 2
		if pos+n > len(raw) {
			return nil, errors.New("tuta: malformed rsa private key")
		}
		parts = append(parts, raw[pos:pos+n])
		pos += n
	}
	return rsaPrivateKeyFromParts(parts)
}

func parseRSAPrivateKeyHex(hexStr string) (*rsa.PrivateKey, error) {
	pos := 0
	var parts [][]byte
	for pos < len(hexStr) {
		if pos+4 > len(hexStr) {
			return nil, errors.New("tuta: malformed rsa private key")
		}
		length, err := parseHexInt(hexStr[pos : pos+4])
		if err != nil {
			return nil, err
		}
		pos += 4
		if pos+length > len(hexStr) {
			return nil, errors.New("tuta: malformed rsa private key")
		}
		comp, err := hex.DecodeString(hexStr[pos : pos+length])
		if err != nil {
			return nil, err
		}
		parts = append(parts, comp)
		pos += length
	}
	return rsaPrivateKeyFromParts(parts)
}

func parseHexInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += int(c - '0')
		case c >= 'a' && c <= 'f':
			n += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int(c-'A') + 10
		default:
			return 0, fmt.Errorf("tuta: invalid hex digit %q", c)
		}
	}
	return n, nil
}

func (s *Session) SendRaw(raw []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm, err := parseOutgoing(raw)
	if err != nil {
		return "", err
	}
	all := make([]mail.Addr, 0, len(pm.to)+len(pm.cc)+len(pm.bcc))
	all = append(all, pm.to...)
	all = append(all, pm.cc...)
	all = append(all, pm.bcc...)
	keys := make(map[string]recipientKey, len(all))
	confidential := true
	for _, rcpt := range all {
		rk, err := s.lookupRecipient(rcpt.Email)
		if err != nil {
			confidential = false
			continue
		}
		keys[rcpt.Email] = rk
	}
	draftList, draftID, sk, err := s.createDraft(pm, confidential)
	if err != nil {
		return "", err
	}
	s.cur = nil
	draft, err := s.rest.getEntity("tutanota", "mail", modelVersionTutanota, draftList, draftID)
	if err != nil {
		return "", err
	}
	params := instance{
		"1791": "en",
		"1790": []any{[]any{draftList, draftID}},
		"1795": "0",
		"1796": "0",
		"1794": nil,
		"1797": []any{},
		"1798": []any{},
		"1799": []any{},
		"1800": []any{},
	}
	var bucketKey []byte
	if confidential {
		bucketKey = make([]byte, 32)
		if _, err := rand.Read(bucketKey); err != nil {
			return "", err
		}
		bucketEnc := encryptKey(bucketKey, sk)
		params["1793"] = b64encode(bucketEnc)
		params["1792"] = nil
	} else {
		params["1792"] = b64encode(sk)
		params["1793"] = nil
	}
	if confidential {
		senderKP, err := s.ownKeyPair()
		if err != nil {
			return "", err
		}
		var internalData []any
		for _, rcpt := range all {
			rk, ok := keys[rcpt.Email]
			if !ok {
				continue
			}
			data, err := s.recipientKeyData(rk, bucketKey, senderKP)
			if err != nil {
				return "", err
			}
			internalData = append(internalData, data)
		}
		params["1797"] = internalData
		if len(internalData) > 0 {
			params["1801"] = encField(sk, "4")
		} else {
			params["1801"] = nil
		}
	} else {
		params["1801"] = nil
	}
	var attachmentData []any
	for _, tuple := range draft.idTuples("115") {
		f, err := s.rest.getEntity("tutanota", "file", modelVersionTutanota, tuple[0], tuple[1])
		if err != nil {
			return "", err
		}
		fileSK, err := s.decryptInstance(f, fileSpec)
		if err != nil {
			return "", err
		}
		if fileSK == nil {
			fileSK, err = s.groupKey(f.str("580"), parseKeyVersion(f.str("1391")))
			if err != nil {
				return "", err
			}
		}
		entry := map[string]any{
			"546": []any{tuple[0], tuple[1]},
		}
		if confidential {
			entry["544"] = b64encode(encryptKey(bucketKey, fileSK))
			entry["545"] = nil
		} else {
			entry["545"] = b64encode(fileSK)
			entry["544"] = nil
		}
		attachmentData = append(attachmentData, entry)
	}
	params["1800"] = attachmentData
	send := instance{
		"548":  "0",
		"549":  params["1791"],
		"550":  params["1792"],
		"551":  params["1793"],
		"552":  params["1794"],
		"675":  params["1795"],
		"1117": params["1796"],
		"1444": params["1801"],
		"1809": nil,
		"1822": "0",
		"553":  params["1797"],
		"554":  params["1798"],
		"1353": params["1799"],
		"555":  params["1800"],
		"556":  params["1790"],
		"1810": params,
	}
	ret, err := s.rest.callService(http.MethodPost, "tutanota", "senddraftservice", modelVersionTutanota, send)
	if err != nil {
		return "", err
	}
	if _, el, ok := ret.idTuple("562"); ok {
		return el, nil
	}
	if mid := ret.str("559"); mid != "" {
		return mid, nil
	}
	return draftID, nil
}

func (s *Session) ownKeyPair() (*groupKeyPair, error) {
	if len(s.mailGroups) == 0 {
		return nil, errors.New("tuta: no mail group")
	}
	return s.groupKeyPair(s.mailGroups[0], 0)
}

func (s *Session) ownKeyVersion() int {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	_, version, err := s.loadCurrentGroupKey(s.mailGroups[0])
	if err != nil {
		return 0
	}
	return version
}

func (s *Session) recipientKeyData(rk recipientKey, bucketKey []byte, senderKP *groupKeyPair) (map[string]any, error) {
	if len(rk.eccPub) > 0 && len(rk.kyberPub) > 0 {
		ephemeralPriv := randomBytes(32)
		pubEnc, err := pqEncapsulate(senderKP.x25519Priv, ephemeralPriv,
			pqPublicKeys{x25519Pub: rk.eccPub, kyberPub: rk.kyberPub}, bucketKey)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"529":  rk.address,
			"530":  b64encode(pubEnc),
			"531":  fmt.Sprint(rk.keyVersion),
			"1352": "2",
			"1431": fmt.Sprint(s.ownKeyVersion()),
		}, nil
	}
	if len(rk.rsaPub) > 0 {
		pub, err := parseRSAPublicKey(rk.rsaPub)
		if err != nil {
			return nil, err
		}
		enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, bucketKey)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"529":  rk.address,
			"530":  b64encode(enc),
			"531":  fmt.Sprint(rk.keyVersion),
			"1352": "0",
			"1431": fmt.Sprint(s.ownKeyVersion()),
		}, nil
	}
	return nil, fmt.Errorf("tuta: recipient %s has no usable public key", rk.address)
}

func (s *Session) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.folderByName(folder)
	if err != nil {
		return 0, err
	}
	if f.kind != "6" {
		return 0, fmt.Errorf("tuta: append is only supported for the drafts folder, not %q", folder)
	}
	pm, err := parseOutgoing(raw)
	if err != nil {
		return 0, err
	}
	confidential := true
	all := make([]mail.Addr, 0, len(pm.to)+len(pm.cc)+len(pm.bcc))
	all = append(all, pm.to...)
	all = append(all, pm.cc...)
	all = append(all, pm.bcc...)
	for _, rcpt := range all {
		if _, err := s.lookupRecipient(rcpt.Email); err != nil {
			confidential = false
			break
		}
	}
	_, draftID, _, err := s.createDraft(pm, confidential)
	if err != nil {
		return 0, err
	}
	s.cur = nil
	return s.st.AssignTutaUID(s.acct.ID, folder, draftID)
}
