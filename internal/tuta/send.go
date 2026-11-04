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

func transferMailAddress(sk []byte, a mail.Addr) map[string]any {
	return map[string]any{
		"2030": genAggID(),
		"2031": b64encode(encryptBytes(sk, []byte(a.Name))),
		"2032": a.Email,
		"2033": []any{},
	}
}

func transferMailAddresses(sk []byte, in []mail.Addr) []any {
	out := make([]any, 0, len(in))
	for _, a := range in {
		out = append(out, transferMailAddress(sk, a))
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
	added := make([]any, 0, len(pm.attachments))
	for _, att := range pm.attachments {
		da, err := s.uploadDraftAttachment(att, groupKey, groupVersion, groupID)
		if err != nil {
			return "", "", nil, err
		}
		added = append(added, da)
	}
	conversationType := "0"
	var previousMessageID any
	if pm.inReplyTo != "" {
		conversationType = "1"
		previousMessageID = pm.inReplyTo
	}
	confVal := "0"
	if confidential {
		confVal = "1"
	}
	encOwner := b64encode(ownerEnc)
	ownerVersion := fmt.Sprint(groupVersion)
	mailTransfer := map[string]any{
		"2059": genAggID(),
		"2060": encOwner,
		"2061": ownerVersion,
		"2062": nil,
		"2063": b64encode(encryptBytes(sk, []byte(pm.subject))),
		"2064": b64encode(encryptBytes(sk, []byte(confVal))),
		"2065": b64encode(encryptBytes(sk, []byte("0"))),
		"2066": []any{transferMailAddress(sk, pm.from)},
		"2067": []any{},
	}
	first := pm.to
	if len(first) == 0 {
		first = pm.cc
	}
	if len(first) == 0 {
		first = pm.bcc
	}
	if len(first) > 0 {
		mailTransfer["2067"] = []any{transferMailAddress(sk, first[0])}
	}
	detailsID := genAggID()
	recipientsID := genAggID()
	bodyID := genAggID()
	blobTransfer := map[string]any{
		"2053": genAggID(),
		"2054": encOwner,
		"2055": ownerVersion,
		"2056": nil,
		"2057": []any{map[string]any{
			"2048": detailsID,
			"2049": []any{},
			"2050": []any{map[string]any{
				"2043": recipientsID,
				"2044": transferMailAddresses(sk, pm.to),
				"2045": transferMailAddresses(sk, pm.cc),
				"2046": transferMailAddresses(sk, pm.bcc),
			}},
			"2051": []any{map[string]any{
				"2039": bodyID,
				"2040": b64encode(encryptBytes(sk, []byte(""))),
				"2041": b64encode(encryptBytes(sk, lz4Compress([]byte(pm.body)))),
			}},
		}},
	}
	data := instance{
		"509":  "0",
		"510":  previousMessageID,
		"511":  conversationType,
		"512":  nil,
		"1427": nil,
		"515": []any{map[string]any{
			"497":  genAggID(),
			"498":  nil,
			"499":  nil,
			"500":  nil,
			"501":  nil,
			"502":  nil,
			"1116": nil,
			"1194": nil,
			"503":  []any{},
			"504":  []any{},
			"505":  []any{},
			"506":  added,
			"507":  []any{},
			"819":  []any{},
			"2069": []any{mailTransfer},
			"2070": []any{blobTransfer},
		}},
	}
	ret, err := s.rest.callService(http.MethodPost, "tutanota", "draftservice", modelVersionSend, data)
	if err != nil {
		return "", "", nil, err
	}
	tuples := ret.idTuples("518")
	if len(tuples) == 0 {
		return "", "", nil, errors.New("tuta: draft service returned no draft id")
	}
	return tuples[0][0], tuples[0][1], sk, nil
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
		tokens = append(tokens, map[string]any{"1991": genAggID(), "1992": tok})
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
		tokens = append(tokens, map[string]any{"1991": genAggID(), "1992": tok})
	}
	fileTransfer := map[string]any{
		"2022": genAggID(),
		"2023": b64encode(encryptKey(groupKey, fileSK)),
		"2024": fmt.Sprint(groupVersion),
		"2025": nil,
		"2026": b64encode(encryptBytes(fileSK, []byte(att.name))),
		"2027": b64encode(encryptBytes(fileSK, []byte(att.mime))),
		"2028": nil,
	}
	if att.cid != "" {
		fileTransfer["2028"] = b64encode(encryptBytes(fileSK, []byte(att.cid)))
	}
	newFile := map[string]any{
		"487":  genAggID(),
		"488":  nil,
		"489":  nil,
		"925":  nil,
		"1226": tokens,
		"2068": []any{fileTransfer},
	}
	return map[string]any{
		"492":  genAggID(),
		"493":  nil,
		"1430": nil,
		"494":  []any{newFile},
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
	sentDone := false
	defer func() {
		if !sentDone {
			_ = s.rest.eraseEntity("tutanota", "mail", modelVersionTutanota, draftList, draftID)
		}
	}()
	s.cur = nil
	draft, err := s.rest.getEntity("tutanota", "mail", modelVersionTutanota, draftList, draftID)
	if err != nil {
		return "", err
	}
	params := instance{
		"1789": genAggID(),
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
		internalData := make([]any, 0, len(all))
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
		tutaCrypt := len(internalData) > 0
		for _, data := range internalData {
			if m, ok := data.(map[string]any); !ok || m["1352"] != "2" {
				tutaCrypt = false
			}
		}
		if tutaCrypt {
			params["1801"] = encField(sk, "4")
		} else {
			params["1801"] = nil
		}
	} else {
		params["1801"] = nil
	}
	attachmentData := make([]any, 0)
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
			"543": genAggID(),
			"546": []any{[]any{tuple[0], tuple[1]}},
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
		"1810": []any{params},
	}
	ret, err := s.rest.callService(http.MethodPost, "tutanota", "senddraftservice", modelVersionSend, send)
	if err != nil {
		return "", err
	}
	sentDone = true
	s.dropFolderCache("Sent")
	s.dropFolderCache("Drafts")
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
			"528":  genAggID(),
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
			"528":  genAggID(),
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
	s.dropFolderCache(folder)
	s.cur = nil
	return s.st.AssignTutaUID(s.acct.ID, folder, draftID)
}
