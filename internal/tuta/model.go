package tuta

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
)

type fieldSpec struct {
	id   string
	kind string
	enc  bool
	agg  *typeSpec
}

type typeSpec struct {
	typ            typeID
	idKey          string
	ownerGroup     string
	ownerEncSK     string
	ownerKeyVer    string
	kdfNonce       string
	bucketKeyAssoc string
	permissions    string
	fields         []fieldSpec
}

var errSessionKeyNotFound = errors.New("tuta: session key not found")

func parseKeyVersion(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func (s *Session) decryptKeyForInstance(encryptingKey, kdfNonce []byte, rootType typeID, data []byte) ([]byte, error) {
	p, err := parseCiphertext(data)
	if err != nil {
		return nil, err
	}
	switch p.kind {
	case kindLegacy, kindCBCUnauthenticated, kindCBCWithMAC:
		return decryptKey(encryptingKey, data)
	case kindAeadGroupKey:
		subKeys := deriveSubKeysFromGroupKey(encryptingKey, kdfNonce, rootType)
		return aeadDecrypt(subKeys, p, []byte("attributeEncGK\x1f"))
	case kindAeadSessionKey:
		subKeys := deriveSubKeysFromSessionKey(encryptingKey, rootType)
		return aeadDecrypt(subKeys, p, []byte("attributeEncSK\x1f"))
	}
	return nil, fmt.Errorf("tuta: unsupported cipher kind %d", p.kind)
}

type valueDecryptor struct {
	sessionKey  []byte
	kdfNonce    []byte
	rootType    typeID
	ownerGroup  string
	ownerKeyVer int
	sess        *Session
}

func (d *valueDecryptor) groupKey() ([]byte, error) {
	return d.sess.groupKey(d.ownerGroup, d.ownerKeyVer)
}

func (d *valueDecryptor) decrypt(fieldPath string, data []byte) ([]byte, error) {
	p, err := parseCiphertext(data)
	if err != nil {
		return nil, err
	}
	switch p.kind {
	case kindLegacy, kindCBCUnauthenticated:
		key := d.sessionKey
		if key == nil {
			gk, err := d.groupKey()
			if err != nil {
				return nil, err
			}
			key = gk
		}
		return aesCBCDecrypt(key, p.iv, p.ciphertext)
	case kindCBCWithMAC:
		if d.sessionKey != nil {
			return decryptCBCWithMAC(d.sessionKey, p)
		}
		gk, err := d.groupKey()
		if err != nil {
			return nil, err
		}
		encKey, macKey := cbcSubKeysFromGroupKey(gk, d.kdfNonce, d.rootType)
		mac := hmac.New(sha256.New, macKey)
		mac.Write(p.iv)
		mac.Write(p.ciphertext)
		if !hmac.Equal(mac.Sum(nil), p.tag) {
			return nil, errors.New("tuta: hmac verification failed")
		}
		return aesCBCDecrypt(encKey, p.iv, p.ciphertext)
	case kindAeadGroupKey:
		gk, err := d.sess.groupKey(d.ownerGroup, int(p.groupKeyVersion))
		if err != nil {
			return nil, err
		}
		subKeys := deriveSubKeysFromGroupKey(gk, d.kdfNonce, d.rootType)
		return aeadDecrypt(subKeys, p, []byte("attributeEncGK\x1f"+fieldPath))
	case kindAeadSessionKey:
		if d.sessionKey == nil {
			return nil, errSessionKeyNotFound
		}
		subKeys := deriveSubKeysFromSessionKey(d.sessionKey, d.rootType)
		return aeadDecrypt(subKeys, p, []byte("attributeEncSK\x1f"+fieldPath))
	}
	return nil, fmt.Errorf("tuta: unsupported cipher kind %d", p.kind)
}

func (s *Session) cachedInstKey(elemID string) ([]byte, bool) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	k, ok := s.instKeys[elemID]
	return k, ok
}

func (s *Session) cacheInstKey(elemID string, key []byte) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	if s.instKeys == nil {
		s.instKeys = make(map[string][]byte)
	}
	s.instKeys[elemID] = key
}

func (s *Session) resolveSessionKey(inst instance, spec *typeSpec) ([]byte, error) {
	if spec.idKey != "" {
		_, elemID, ok := inst.idTuple(spec.idKey)
		if !ok {
			elemID = inst.str(spec.idKey)
		}
		if elemID != "" {
			if k, hit := s.cachedInstKey(elemID); hit {
				return k, nil
			}
		}
	}
	if spec.bucketKeyAssoc != "" {
		if bk := inst.agg(spec.bucketKeyAssoc); bk != nil {
			return s.resolveWithBucketKey(inst, spec, bk)
		}
	}
	if spec.ownerEncSK != "" {
		if enc := inst.bytes(spec.ownerEncSK); len(enc) > 0 {
			ownerGroup := inst.str(spec.ownerGroup)
			version := parseKeyVersion(inst.str(spec.ownerKeyVer))
			gk, err := s.groupKey(ownerGroup, version)
			if err != nil {
				return nil, err
			}
			return decryptKey(gk, enc)
		}
	}
	if spec.permissions != "" {
		if listID := inst.str(spec.permissions); listID != "" {
			return s.resolveViaPermissions(listID)
		}
	}
	return nil, nil
}

func (s *Session) resolveViaPermissions(listID string) ([]byte, error) {
	perms, err := s.rest.loadRange("sys", "permission", modelVersionSys, listID, "", 100, false)
	if err != nil {
		return nil, err
	}
	for _, p := range perms {
		typ := p.str("137")
		if typ != "1" && typ != "2" {
			continue
		}
		group := p.str("1002")
		enc := p.bytes("1003")
		if group == "" || len(enc) == 0 {
			continue
		}
		gk, err := s.groupKey(group, parseKeyVersion(p.str("2242")))
		if err != nil {
			continue
		}
		return decryptKey(gk, enc)
	}
	return nil, errSessionKeyNotFound
}

func (s *Session) resolveWithBucketKey(inst instance, spec *typeSpec, bk instance) ([]byte, error) {
	var bucketKey []byte
	keyGroup := bk.assocID("2047")
	recipientKeyVersion := parseKeyVersion(bk.str("2252"))
	if pubEnc := bk.bytes("2045"); len(pubEnc) > 0 {
		kp, err := s.groupKeyPair(keyGroup, recipientKeyVersion)
		if err != nil {
			return nil, err
		}
		switch protocol := bk.str("2158"); protocol {
		case "2":
			bkKey, _, err := pqDecapsulate(pubEnc,
				pqKeyPairs{x25519Priv: kp.x25519Priv, kyberPriv: kp.kyberPriv},
				pqPublicKeys{x25519Pub: kp.x25519Pub, kyberPub: kp.kyberPub})
			if err != nil {
				return nil, err
			}
			bucketKey = bkKey
		case "0":
			if kp.rsaPriv == nil {
				return nil, errors.New("tuta: no rsa private key available for bucket key")
			}
			bkKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, kp.rsaPriv, pubEnc, nil)
			if err != nil {
				return nil, err
			}
			bucketKey = bkKey
		default:
			return nil, fmt.Errorf("tuta: unsupported bucket key protocol version %q", protocol)
		}
	} else if groupEnc := bk.bytes("2046"); len(groupEnc) > 0 {
		if keyGroup == "" {
			keyGroup = inst.str(spec.ownerGroup)
		}
		gk, err := s.groupKey(keyGroup, recipientKeyVersion)
		if err != nil {
			return nil, err
		}
		bkKey, err := decryptKey(gk, groupEnc)
		if err != nil {
			return nil, err
		}
		bucketKey = bkKey
	} else {
		return nil, errSessionKeyNotFound
	}
	_, elemID, ok := inst.idTuple(spec.idKey)
	if !ok {
		elemID = inst.str(spec.idKey)
	}
	s.keysMu.Lock()
	if s.instKeys == nil {
		s.instKeys = make(map[string][]byte)
	}
	for _, isk := range bk.aggList("2048") {
		id := isk.str("2041")
		if id == "" {
			continue
		}
		if _, done := s.instKeys[id]; done {
			continue
		}
		if k, err := decryptKey(bucketKey, isk.bytes("2042")); err == nil {
			s.instKeys[id] = k
		}
	}
	cached := s.instKeys[elemID]
	s.keysMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	return nil, errSessionKeyNotFound
}

func (s *Session) decryptInstance(inst instance, spec *typeSpec) ([]byte, error) {
	sk, err := s.resolveSessionKey(inst, spec)
	if err != nil {
		return nil, err
	}
	d := &valueDecryptor{
		sessionKey:  sk,
		kdfNonce:    inst.bytes(spec.kdfNonce),
		rootType:    spec.typ,
		ownerGroup:  inst.str(spec.ownerGroup),
		ownerKeyVer: parseKeyVersion(inst.str(spec.ownerKeyVer)),
		sess:        s,
	}
	if err := s.decryptFields(inst, spec, d, ""); err != nil {
		return sk, err
	}
	return sk, nil
}

func (s *Session) decryptFields(inst instance, spec *typeSpec, d *valueDecryptor, prefix string) error {
	for _, f := range spec.fields {
		path := f.id
		if prefix != "" {
			path = prefix + "/" + f.id
		}
		if f.agg != nil {
			switch v := inst[f.id].(type) {
			case map[string]any:
				if err := s.decryptFields(instance(v), f.agg, d, path); err != nil {
					return err
				}
			case []any:
				for _, item := range v {
					if m, ok := item.(map[string]any); ok {
						if err := s.decryptFields(instance(m), f.agg, d, path); err != nil {
							return err
						}
					}
				}
			}
			continue
		}
		if !f.enc {
			continue
		}
		raw := inst.bytes(f.id)
		if raw == nil {
			continue
		}
		plain, err := d.decrypt(path, raw)
		if err != nil {
			return fmt.Errorf("tuta: decrypt %s field %s: %w", spec.typ, path, err)
		}
		switch f.kind {
		case "Bytes":
			inst[f.id] = b64encode(plain)
		case "CompressedString":
			dec, err := lz4Decompress(plain)
			if err != nil {
				return fmt.Errorf("tuta: decompress %s field %s: %w", spec.typ, path, err)
			}
			inst[f.id] = string(dec)
		default:
			inst[f.id] = string(plain)
		}
	}
	return nil
}
