package tuta

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func errResponse(r *http.Request, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(`{"errorId":"x","precondition":"y"}`)),
		Header:     make(http.Header),
		Request:    r,
	}
}

func sendCleanupSession(t *testing.T, failDraftFetch bool, deletes *[]string) *Session {
	t.Helper()
	userGroupKey := bytes.Repeat([]byte{0x11}, 32)
	mailKey := bytes.Repeat([]byte{0x22}, 32)
	symEncMail := b64encode(encryptKey(userGroupKey, mailKey))
	s := newSession()
	s.userID = "user1"
	s.userGroupID = "userGroup1"
	s.userGroupKey = userGroupKey
	s.userKeyVersion = 0
	s.mailGroups = []string{"mailGroup1"}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodDelete && strings.Contains(p, "/tutanota/mail/"):
			*deletes = append(*deletes, p)
			return jsonResponse(r, `{}`), nil
		case strings.Contains(p, "/sys/user/"):
			return jsonResponse(r, fmt.Sprintf(`{"95":[{"29":["userGroup1"],"27":"AAAA","2246":"0","2247":"0","1030":"0"}],"96":[{"29":["mailGroup1"],"27":"%s","2246":"1","2247":"0","1030":"5"}]}`, symEncMail)), nil
		case strings.Contains(p, "/sys/publickeyservice"):
			return errResponse(r, 500), nil
		case strings.Contains(p, "/tutanota/draftservice"):
			return jsonResponse(r, `{"518":["draftList1","draftElem1"]}`), nil
		case strings.Contains(p, "/tutanota/mail/"):
			if failDraftFetch {
				return errResponse(r, 500), nil
			}
			return jsonResponse(r, `{"99":["draftList1","draftElem1"]}`), nil
		case strings.Contains(p, "/tutanota/senddraftservice"):
			return jsonResponse(r, `{"562":["sentList1","sentElem1"]}`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	return s
}

const sendCleanupRaw = "From: a@tuta.test\r\nTo: bob@example.com\r\nSubject: cleanup probe\r\n\r\nhello body\r\n"

func TestSendRawFailureErasesCreatedDraft(t *testing.T) {
	var deletes []string
	s := sendCleanupSession(t, true, &deletes)
	if _, err := s.SendRaw([]byte(sendCleanupRaw)); err == nil {
		t.Fatal("SendRaw succeeded unexpectedly; the draft fetch was rigged to fail")
	}
	want := "/rest/tutanota/mail/draftList1/draftElem1"
	if len(deletes) != 1 || deletes[0] != want {
		t.Fatalf("draft erase calls = %v, want exactly [%s]", deletes, want)
	}
}

func TestSendRawSuccessKeepsSentMessage(t *testing.T) {
	var deletes []string
	s := sendCleanupSession(t, false, &deletes)
	id, err := s.SendRaw([]byte(sendCleanupRaw))
	if err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	if id != "sentElem1" {
		t.Fatalf("sent id = %q, want sentElem1", id)
	}
	if len(deletes) != 0 {
		t.Fatalf("erase calls on success = %v, want none", deletes)
	}
}
