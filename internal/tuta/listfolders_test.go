package tuta

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func TestListFoldersChain(t *testing.T) {
	s := newSession()
	s.mailGroups = []string{"mailGroup1"}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/mailboxgrouproot/"):
			return jsonResponse(r, `{"695":"root1","698":["mailGroup1"],"699":["mailbox1"]}`), nil
		case strings.Contains(r.URL.Path, "/mailbox/"):
			return jsonResponse(r, `{"127":"mailbox1","443":[{"441":"refId","442":["setsList1"]}]}`), nil
		case strings.Contains(r.URL.Path, "/mailset/"):
			return jsonResponse(r, `[
				{"431":["setsList1","setInbox"],"433":"0","436":"1","1459":["entriesInbox"]},
				{"431":["setsList1","setSent"],"433":"0","436":"2","1459":["entriesSent"]},
				{"431":["setsList1","setCustom"],"433":"0","436":"0","1459":["entriesCustom"]},
				{"431":["setsList1","setAll"],"433":"0","436":"7","1459":["entriesAll"]}
			]`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	folders, err := s.listFolders()
	if err != nil {
		t.Fatalf("listFolders: %v", err)
	}
	if len(folders) != 3 {
		t.Fatalf("folders = %d, want 3 (ALL set skipped)", len(folders))
	}
	want := []tutaFolder{
		{name: "Inbox", special: "inbox", kind: "1", setID: "setInbox", entriesID: "entriesInbox"},
		{name: "Sent", special: "sent", kind: "2", setID: "setSent", entriesID: "entriesSent"},
		{name: "setCustom", kind: "0", setID: "setCustom", entriesID: "entriesCustom"},
	}
	for i, w := range want {
		f := folders[i]
		if f.name != w.name || f.special != w.special || f.kind != w.kind || f.setID != w.setID || f.entriesID != w.entriesID {
			t.Fatalf("folder %d = %+v, want name/special/kind/setID/entriesID from %+v", i, f, w)
		}
		if f.groupID != "mailGroup1" || f.setListID != "setsList1" {
			t.Fatalf("folder %d group/list = %q/%q, want mailGroup1/setsList1", i, f.groupID, f.setListID)
		}
	}
}
