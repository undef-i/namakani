package tuta

import (
	"encoding/base64"
	"fmt"
	"strconv"
)

func keyVersionCustomID(version int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(version)))
}

func decodeKeyVersionCustomID(id string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(id)
		if err != nil {
			return 0, err
		}
	}
	return strconv.Atoi(string(raw))
}

func (s *Session) groupKey(groupID string, version int) ([]byte, error) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	return s.groupKeyLocked(groupID, version)
}

func (s *Session) groupKeyLocked(groupID string, version int) ([]byte, error) {
	if groupID == s.userGroupID && s.userGroupKey != nil {
		if version == 0 || version == s.userKeyVersion {
			return s.userGroupKey, nil
		}
	}
	if byVer, ok := s.groupKeys[groupID]; ok {
		if version == 0 {
			best := -1
			for v := range byVer {
				if v > best {
					best = v
				}
			}
			if best >= 0 {
				return byVer[best], nil
			}
		} else if k, ok := byVer[version]; ok {
			return k, nil
		}
	}
	current, currentVersion, err := s.loadCurrentGroupKey(groupID)
	if err != nil {
		return nil, err
	}
	s.cacheGroupKeyLocked(groupID, currentVersion, current)
	if version == 0 || version == currentVersion {
		return current, nil
	}
	if version > currentVersion {
		return nil, fmt.Errorf("tuta: group key version %d newer than current %d for group %s", version, currentVersion, groupID)
	}
	formers, err := s.loadFormerGroupKeys(groupID, currentVersion, currentVersion-version)
	if err != nil {
		return nil, err
	}
	byVersion := make(map[int][]byte, len(formers))
	for _, fk := range formers {
		byVersion[fk.version] = fk.ownerEncGKey
	}
	key := current
	for v := currentVersion - 1; v >= version; v-- {
		enc, ok := byVersion[v]
		if !ok {
			return nil, fmt.Errorf("tuta: former group key version %d not found for group %s", v, groupID)
		}
		next, err := decryptKey(key, enc)
		if err != nil {
			return nil, err
		}
		key = next
		s.cacheGroupKeyLocked(groupID, v, key)
	}
	return key, nil
}

func (s *Session) cacheGroupKey(groupID string, version int, key []byte) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	s.cacheGroupKeyLocked(groupID, version, key)
}

func (s *Session) cacheGroupKeyLocked(groupID string, version int, key []byte) {
	byVer, ok := s.groupKeys[groupID]
	if !ok {
		byVer = make(map[int][]byte)
		s.groupKeys[groupID] = byVer
	}
	byVer[version] = key
}

type membership struct {
	groupID    string
	symEncGKey []byte
	version    int
	symKeyVer  int
	groupType  string
}

func (s *Session) loadUser() (instance, []membership, string, error) {
	user, err := s.rest.getEntity("sys", "user", modelVersionSys, s.userID)
	if err != nil {
		return nil, nil, "", err
	}
	var userGroupID string
	if ug := user.agg("95"); ug != nil {
		userGroupID = ug.str("29")
	}
	var out []membership
	for _, m := range user.aggList("96") {
		out = append(out, membership{
			groupID:    m.str("29"),
			symEncGKey: m.bytes("27"),
			version:    parseKeyVersion(m.str("2246")),
			symKeyVer:  parseKeyVersion(m.str("2247")),
			groupType:  m.str("1030"),
		})
	}
	return user, out, userGroupID, nil
}

func (s *Session) loadCurrentGroupKey(groupID string) ([]byte, int, error) {
	_, mems, _, err := s.loadUser()
	if err != nil {
		return nil, 0, err
	}
	for _, m := range mems {
		if m.groupID == groupID {
			ugk, err := s.userGroupKeyAtLocked(m.symKeyVer)
			if err != nil {
				return nil, 0, err
			}
			key, err := decryptKey(ugk, m.symEncGKey)
			if err != nil {
				return nil, 0, err
			}
			return key, m.version, nil
		}
	}
	return nil, 0, fmt.Errorf("tuta: no membership for group %s", groupID)
}

func (s *Session) userGroupKeyAtLocked(version int) ([]byte, error) {
	if version == 0 || version == s.userKeyVersion {
		return s.userGroupKey, nil
	}
	return s.groupKeyLocked(s.userGroupID, version)
}

type formerGroupKey struct {
	version      int
	ownerEncGKey []byte
}

func (s *Session) formerKeysListID(groupID string) (string, error) {
	group, err := s.rest.getEntity("sys", "group", modelVersionSys, groupID)
	if err != nil {
		return "", err
	}
	ref := group.agg("2273")
	if ref == nil {
		return "", fmt.Errorf("tuta: group %s has no former keys ref", groupID)
	}
	return ref.str("2269"), nil
}

func (s *Session) loadFormerGroupKeys(groupID string, currentVersion, count int) ([]formerGroupKey, error) {
	listID, err := s.formerKeysListID(groupID)
	if err != nil {
		return nil, err
	}
	items, err := s.rest.loadRange("sys", "groupkey", modelVersionSys, listID, keyVersionCustomID(currentVersion), count, true)
	if err != nil {
		return nil, err
	}
	out := make([]formerGroupKey, 0, len(items))
	for _, it := range items {
		ver, err := decodeKeyVersionCustomID(it.str("2257"))
		if err != nil {
			continue
		}
		out = append(out, formerGroupKey{version: ver, ownerEncGKey: it.bytes("2261")})
	}
	return out, nil
}

func (s *Session) groupKeyPair(groupID string, version int) (*groupKeyPair, error) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	if byVer, ok := s.keyPairs[groupID]; ok {
		if version == 0 {
			best := -1
			for v := range byVer {
				if v > best {
					best = v
				}
			}
			if best >= 0 {
				return byVer[best], nil
			}
		} else if kp, ok := byVer[version]; ok {
			return kp, nil
		}
	}
	group, err := s.rest.getEntity("sys", "group", modelVersionSys, groupID)
	if err != nil {
		return nil, err
	}
	currentVersion := parseKeyVersion(group.str("2271"))
	var kpInst instance
	kpVersion := version
	if version == 0 || version == currentVersion {
		kpInst = group.agg("13")
		kpVersion = currentVersion
	} else {
		listID, err := s.formerKeysListID(groupID)
		if err != nil {
			return nil, err
		}
		fk, err := s.rest.getEntity("sys", "groupkey", modelVersionSys, listID, keyVersionCustomID(version))
		if err != nil {
			return nil, err
		}
		kpInst = fk.agg("2266")
	}
	if kpInst == nil {
		return nil, fmt.Errorf("tuta: no key pair for group %s version %d", groupID, kpVersion)
	}
	decryptingKey, err := s.groupKeyLocked(groupID, kpVersion)
	if err != nil {
		return nil, err
	}
	kp := &groupKeyPair{
		x25519Pub: kpInst.bytes("2144"),
		kyberPub:  kpInst.bytes("2146"),
		rsaPub:    kpInst.bytes("2"),
	}
	if enc := kpInst.bytes("2145"); len(enc) > 0 {
		priv, err := decryptKey(decryptingKey, enc)
		if err != nil {
			return nil, err
		}
		kp.x25519Priv = priv
	}
	if enc := kpInst.bytes("2147"); len(enc) > 0 {
		priv, err := decryptKey(decryptingKey, enc)
		if err != nil {
			return nil, err
		}
		kp.kyberPriv = priv
	}
	byVer, ok := s.keyPairs[groupID]
	if !ok {
		byVer = make(map[int]*groupKeyPair)
		s.keyPairs[groupID] = byVer
	}
	byVer[kpVersion] = kp
	return kp, nil
}
