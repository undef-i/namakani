package tuta

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCreateDraftUsesTransferAggregates(t *testing.T) {
	userGroupKey := bytes.Repeat([]byte{0x11}, 32)
	mailKey := bytes.Repeat([]byte{0x22}, 32)
	symEncMail := b64encode(encryptKey(userGroupKey, mailKey))
	var draftBody []byte
	s := newSession()
	s.userID = "user1"
	s.userGroupID = "userGroup1"
	s.userGroupKey = userGroupKey
	s.userKeyVersion = 0
	s.mailGroups = []string{"mailGroup1"}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/sys/user/"):
			return jsonResponse(r, fmt.Sprintf(`{"95":[{"29":["userGroup1"],"27":"AAAA","2246":"0","2247":"0","1030":"0"}],"96":[{"29":["mailGroup1"],"27":"%s","2246":"1","2247":"0","1030":"5"}]}`, symEncMail)), nil
		case strings.Contains(p, "/tutanota/draftservice"):
			draftBody, _ = io.ReadAll(r.Body)
			return jsonResponse(r, `{"518":[["draftList1","draftElem1"]]}`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	pm, err := parseOutgoing([]byte("From: Alice <a@tuta.test>\r\nTo: Bob <bob@example.com>\r\nCc: carol@example.com\r\nSubject: hello subj\r\n\r\nline one\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, sk, err := s.createDraft(pm, false)
	if err != nil {
		t.Fatalf("createDraft: %v", err)
	}
	top, err := decodeInstance(draftBody)
	if err != nil {
		t.Fatal(err)
	}
	if top["512"] != nil || top["1427"] != nil {
		t.Fatalf("deprecated create fields not null: %v %v", top["512"], top["1427"])
	}
	dd := top.agg("515")
	if dd == nil {
		t.Fatal("no draftData")
	}
	for _, k := range []string{"498", "499", "500", "501", "502", "1116", "1194"} {
		if dd[k] != nil {
			t.Fatalf("deprecated draftData field %s = %v, want null", k, dd[k])
		}
	}
	mailTr := dd.agg("2069")
	if mailTr == nil {
		t.Fatal("no mail transfer")
	}
	dMail := &valueDecryptor{sessionKey: sk, rootType: mailSpec.typ}
	subj, err := dMail.decrypt("105", mailTr.bytes("2063"))
	if err != nil || string(subj) != "hello subj" {
		t.Fatalf("subject round trip: %q %v", subj, err)
	}
	conf, err := dMail.decrypt("426", mailTr.bytes("2064"))
	if err != nil || string(conf) != "0" {
		t.Fatalf("confidential round trip: %q %v", conf, err)
	}
	sender := mailTr.agg("2066")
	name, err := dMail.decrypt("111/"+sender.str("2030")+"/94", sender.bytes("2031"))
	if err != nil || string(name) != "Alice" {
		t.Fatalf("sender name round trip: %q %v", name, err)
	}
	if sender.str("2032") != "a@tuta.test" {
		t.Fatalf("sender address = %q", sender.str("2032"))
	}
	ownerSK, err := decryptKey(mailKey, mailTr.bytes("2060"))
	if err != nil || !bytes.Equal(ownerSK, sk) {
		t.Fatalf("ownerEncSessionKey round trip: %v", err)
	}
	blobTr := dd.agg("2070")
	if blobTr == nil {
		t.Fatal("no blob transfer")
	}
	details := blobTr.agg("2057")
	dBlob := &valueDecryptor{sessionKey: sk, rootType: mailDetailsBlobSpec.typ}
	recips := details.agg("2050")
	toList := recips.aggList("2044")
	if len(toList) != 1 {
		t.Fatalf("to recipients = %d", len(toList))
	}
	toPath := "1305/" + details.str("2048") + "/1286/" + recips.str("2043") + "/1279/" + toList[0].str("2030") + "/94"
	toName, err := dBlob.decrypt(toPath, toList[0].bytes("2031"))
	if err != nil || string(toName) != "Bob" {
		t.Fatalf("to name round trip: %q %v (path %s)", toName, err, toPath)
	}
	body := details.agg("2051")
	bodyPath := "1305/" + details.str("2048") + "/1288/" + body.str("2039") + "/1276"
	comp, err := dBlob.decrypt(bodyPath, body.bytes("2041"))
	if err != nil {
		t.Fatalf("body decrypt: %v", err)
	}
	plain, err := lz4Decompress(comp)
	if err != nil || !strings.Contains(string(plain), "line one") {
		t.Fatalf("body round trip: %q %v", plain, err)
	}
}

