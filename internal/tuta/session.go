package tuta

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (s *Session) derivePassphraseKey(email, password string) ([]byte, int, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	ret, err := s.rest.callService(http.MethodGet, "sys", "saltservice", modelVersionSys, instance{
		"418": "0",
		"419": email,
	})
	if err != nil {
		return nil, 0, err
	}
	salt := ret.bytes("422")
	kdfVersion := ret.str("2133")
	switch kdfVersion {
	case "0", "":
		key, err := passphraseKeyBcrypt(password, salt, 128)
		return key, 0, err
	case "1":
		return passphraseKeyArgon2id(password, salt), 1, nil
	}
	return nil, 0, fmt.Errorf("tuta: unsupported kdf version %q", kdfVersion)
}

func (s *Session) finishUnlock() error {
	_, mems, userGroupID, err := s.loadUser()
	if err != nil {
		return err
	}
	s.userGroupID = userGroupID
	for _, m := range mems {
		if m.groupID == userGroupID {
			key, err := decryptKey(s.passphraseKey, m.symEncGKey)
			if err != nil {
				return fmt.Errorf("tuta: unlock user group key: %w", err)
			}
			s.userGroupKey = key
			s.userKeyVersion = m.version
		}
		if m.groupType == "5" {
			s.mailGroups = append(s.mailGroups, m.groupID)
		}
	}
	if s.userGroupKey == nil {
		return errors.New("tuta: user group membership not found")
	}
	s.cacheGroupKey(userGroupID, s.userKeyVersion, s.userGroupKey)
	return nil
}

func (s *Session) login(password, totp string) error {
	email := strings.ToLower(strings.TrimSpace(s.acct.Email))
	passphraseKey, _, err := s.derivePassphraseKey(email, password)
	if err != nil {
		return err
	}
	accessKey := make([]byte, 32)
	if _, err := rand.Read(accessKey); err != nil {
		return err
	}
	ret, err := s.rest.callService(http.MethodPost, "sys", "sessionservice", modelVersionSys, instance{
		"1212": "0",
		"1213": email,
		"1214": authVerifier(passphraseKey),
		"1215": "namakani",
		"1216": b64encode(accessKey),
		"1217": nil,
		"1417": nil,
		"1218": []any{},
	})
	if err != nil {
		return err
	}
	accessToken := ret.str("1221")
	userAssoc := ret.str("1223")
	s.rest.accessToken = accessToken
	s.accessToken = accessToken
	if userAssoc != "" {
		s.userID = userAssoc
	}
	challenges := ret.aggList("1222")
	if len(challenges) > 0 {
		if err := s.completeSecondFactor(challenges, accessToken, totp); err != nil {
			return err
		}
	}
	if s.userID == "" {
		return errors.New("tuta: login response missing user id")
	}
	s.passphraseKey = passphraseKey
	if err := s.finishUnlock(); err != nil {
		return err
	}
	s.persistSession()
	return nil
}

func (s *Session) completeSecondFactor(challenges []instance, accessToken, totp string) error {
	var totpChallenge bool
	for _, ch := range challenges {
		if ch.str("1189") == "1" {
			totpChallenge = true
		}
	}
	if !totpChallenge {
		return errors.New("tuta: unsupported second factor challenge (webauthn/u2f)")
	}
	if totp == "" {
		return ErrTOTPRequired
	}
	user, err := s.rest.getEntity("sys", "user", modelVersionSys, s.userID)
	if err != nil {
		return err
	}
	sessionsList := ""
	if auth := user.agg("1210"); auth != nil {
		sessionsList = auth.str("1208")
	}
	if sessionsList == "" {
		return errors.New("tuta: session list not found for second factor auth")
	}
	_, err = s.rest.callService(http.MethodPost, "sys", "secondfactorauthservice", modelVersionSys, instance{
		"542":  "0",
		"1230": "1",
		"1243": totp,
		"1232": []any{sessionsList, accessToken},
	})
	return err
}

func (s *Session) resume() error {
	if s.st == nil || s.acct.ID == 0 {
		return errors.New("tuta: no stored session")
	}
	stored, err := s.st.LoadTutaSession(s.acct.ID)
	if err != nil || stored == nil {
		return errors.New("tuta: no stored session")
	}
	s.rest.accessToken = stored.AccessToken
	s.accessToken = stored.AccessToken
	s.userID = stored.UserID
	passphraseKey, _, err := s.derivePassphraseKey(s.acct.Email, s.acct.Password)
	if err != nil {
		s.rest.accessToken = ""
		return err
	}
	s.passphraseKey = passphraseKey
	if err := s.finishUnlock(); err != nil {
		s.rest.accessToken = ""
		s.accessToken = ""
		return err
	}
	return nil
}

func (s *Session) persistSession() {
	if s.st == nil || s.acct.ID == 0 {
		return
	}
	_ = s.st.SaveTutaSession(s.acct.ID, s.userID, s.accessToken)
}
