package tuta

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestResolveSessionKeyVersionZero(t *testing.T) {
	userGroupKey := bytes.Repeat([]byte{0x11}, 32)
	mailKeyV1 := bytes.Repeat([]byte{0x22}, 32)
	mailKeyV0 := bytes.Repeat([]byte{0x33}, 16)
	sessionKey := bytes.Repeat([]byte{0x44}, 16)

	symEncMailV1 := b64encode(encryptKey(userGroupKey, mailKeyV1))
	ownerEncV0 := b64encode(encryptKey(mailKeyV1, mailKeyV0))
	encSK := encryptKey(mailKeyV0, sessionKey)
	if len(encSK) != 16 {
		t.Fatalf("owner enc session key = %d bytes, want 16 (single fixed-IV block)", len(encSK))
	}

	s := newSession()
	s.userID = "user1"
	s.userGroupID = "userGroup1"
	s.userGroupKey = userGroupKey
	s.userKeyVersion = 0
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/sys/user/"):
			return jsonResponse(r, fmt.Sprintf(`{"95":[{"29":["userGroup1"],"27":"AAAA","2246":"0","2247":"0","1030":"0"}],"96":[{"29":["mailGroup1"],"27":"%s","2246":"1","2247":"0","1030":"5"}]}`, symEncMailV1)), nil
		case strings.Contains(r.URL.Path, "/sys/groupkey/"):
			return jsonResponse(r, fmt.Sprintf(`[{"2257":["formerList1","MA"],"2261":"%s"}]`, ownerEncV0)), nil
		case strings.Contains(r.URL.Path, "/sys/group/"):
			return jsonResponse(r, `{"2271":"1","2273":[{"2269":["formerList1"]}]}`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}

	set := instance{
		"431":  []any{"setsList1", "setInbox"},
		"589":  "mailGroup1",
		"1399": "0",
		"434":  b64encode(encSK),
	}
	sk, err := s.resolveSessionKey(set, mailSetSpec)
	if err != nil {
		t.Fatalf("resolveSessionKey: %v", err)
	}
	if !bytes.Equal(sk, sessionKey) {
		t.Fatalf("session key = %x, want %x", sk, sessionKey)
	}
}
