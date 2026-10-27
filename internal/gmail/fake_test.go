package gmail

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeCall struct {
	Method string
	Path   string
	Body   string
}

type fakeLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type fakeMsg struct {
	id           string
	threadID     string
	labels       map[string]bool
	subject      string
	from         string
	to           string
	dateHdr      string
	internalDate int64
	raw          string
	hasAttach    bool
}

type fakeHistChange struct {
	msgID  string
	labels []string
}

type fakeHistRecord struct {
	id            int64
	added         []string
	deleted       []string
	labelsAdded   []fakeHistChange
	labelsRemoved []fakeHistChange
}

type fakeGmail struct {
	mu             sync.Mutex
	msgs           map[string]*fakeMsg
	labels         []fakeLabel
	historySeq     int64
	historyValidID int64
	records        []fakeHistRecord
	calls          []fakeCall
	tokenCalls     int
	expiresIn      int
	refreshToken   string
	rotateTo       string
	codeRedeemed   bool
	profileEmail   string
	srv            *httptest.Server
}

func newFakeGmail(t *testing.T) *fakeGmail {
	t.Helper()
	f := &fakeGmail{
		msgs:         map[string]*fakeMsg{},
		historySeq:   1000,
		refreshToken: "rt-1",
		expiresIn:    3600,
		profileEmail: "user@gmail.com",
		labels: []fakeLabel{
			{ID: "INBOX", Name: "INBOX", Type: "system"},
			{ID: "SENT", Name: "SENT", Type: "system"},
			{ID: "DRAFT", Name: "DRAFT", Type: "system"},
			{ID: "SPAM", Name: "SPAM", Type: "system"},
			{ID: "TRASH", Name: "TRASH", Type: "system"},
			{ID: "STARRED", Name: "STARRED", Type: "system"},
			{ID: "IMPORTANT", Name: "IMPORTANT", Type: "system"},
			{ID: "UNREAD", Name: "UNREAD", Type: "system"},
			{ID: "CATEGORY_SOCIAL", Name: "CATEGORY_SOCIAL", Type: "system"},
			{ID: "Label_1", Name: "Work", Type: "user"},
			{ID: "Label_2", Name: "Work/Projects", Type: "user"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/gmail/v1/users/me/", f.handleAPI)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGmail) use(t *testing.T) {
	t.Helper()
	oldAPI, oldToken := APIBase, TokenURL
	APIBase = f.srv.URL + "/gmail/v1/users/me"
	TokenURL = f.srv.URL + "/token"
	t.Cleanup(func() { APIBase, TokenURL = oldAPI, oldToken })
}

func (f *fakeGmail) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.tokenCalls++
	grant := r.Form.Get("grant_type")
	rotate := f.rotateTo
	f.rotateTo = ""
	f.mu.Unlock()
	resp := map[string]any{"access_token": "at-1", "expires_in": f.expiresIn, "token_type": "Bearer"}
	switch grant {
	case "refresh_token":
		if r.Form.Get("refresh_token") != f.refreshToken {
			writeFakeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "Token has been expired or revoked."})
			return
		}
		if rotate != "" {
			resp["refresh_token"] = rotate
			f.mu.Lock()
			f.refreshToken = rotate
			f.mu.Unlock()
		}
	case "authorization_code":
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			writeFakeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "Bad code."})
			return
		}
		f.mu.Lock()
		f.codeRedeemed = true
		f.mu.Unlock()
		resp["refresh_token"] = f.refreshToken
	default:
		writeFakeErr(w, 400, "unsupported_grant_type", "nope")
		return
	}
	writeFakeJSON(w, 200, resp)
}

func writeFakeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeFakeErr(w http.ResponseWriter, code int, reason, msg string) {
	writeFakeJSON(w, code, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "status": reason,
		"errors": []map[string]any{{"reason": reason, "message": msg}},
	}})
}

func (f *fakeGmail) handleAPI(w http.ResponseWriter, r *http.Request) {
	body := ""
	if r.Body != nil {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := r.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		body = string(buf)
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: r.Method, Path: r.URL.Path + "?" + r.URL.RawQuery, Body: body})
	f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		writeFakeErr(w, 401, "authError", "Missing token.")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me")
	q := r.URL.Query()
	switch {
	case rest == "/profile" && r.Method == http.MethodGet:
		f.mu.Lock()
		hid := f.historySeq
		n := len(f.msgs)
		f.mu.Unlock()
		writeFakeJSON(w, 200, map[string]any{
			"emailAddress": f.profileEmail, "messagesTotal": n, "threadsTotal": n,
			"historyId": strconv.FormatInt(hid, 10),
		})
	case rest == "/labels" && r.Method == http.MethodGet:
		writeFakeJSON(w, 200, map[string]any{"labels": f.labels})
	case rest == "/messages" && r.Method == http.MethodGet:
		f.listMessages(w, q)
	case rest == "/messages/send" && r.Method == http.MethodPost:
		f.sendMessage(w, body)
	case strings.HasPrefix(rest, "/messages/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(rest, "/messages/")
		f.getMessage(w, id, q.Get("format"))
	case strings.HasPrefix(rest, "/messages/") && strings.HasSuffix(rest, "/modify") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "/messages/"), "/modify")
		f.modifyMessage(w, id, body)
	case strings.HasPrefix(rest, "/messages/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(rest, "/messages/")
		f.deleteMessage(w, id)
	case rest == "/drafts" && r.Method == http.MethodPost:
		f.createDraft(w, body)
	case rest == "/history" && r.Method == http.MethodGet:
		f.listHistory(w, q)
	default:
		writeFakeErr(w, 404, "notFound", "no route "+rest)
	}
}

func (f *fakeGmail) listMessages(w http.ResponseWriter, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	wantLabels := q["labelIds"]
	query := get("q")
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, m := range f.msgs {
		ok := true
		for _, l := range wantLabels {
			if !m.labels[l] {
				ok = false
				break
			}
		}
		if ok && query == "-in:spam -in:trash" && (m.labels["SPAM"] || m.labels["TRASH"]) {
			ok = false
		}
		if ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return f.msgs[ids[i]].internalDate > f.msgs[ids[j]].internalDate })
	pageSize := 500
	if v := get("maxResults"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	start := 0
	if tok := get("pageToken"); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	end := start + pageSize
	if end > len(ids) {
		end = len(ids)
	}
	out := map[string]any{"resultSizeEstimate": len(ids)}
	var page []map[string]any
	for _, id := range ids[start:end] {
		page = append(page, map[string]any{"id": id, "threadId": f.msgs[id].threadID})
	}
	if page != nil {
		out["messages"] = page
	}
	if end < len(ids) {
		out["nextPageToken"] = strconv.Itoa(end)
	}
	writeFakeJSON(w, 200, out)
}

func (f *fakeGmail) metaJSON(m *fakeMsg) map[string]any {
	var labelIDs []string
	for l := range m.labels {
		labelIDs = append(labelIDs, l)
	}
	sort.Strings(labelIDs)
	headers := []map[string]string{
		{"name": "Subject", "value": m.subject},
		{"name": "From", "value": m.from},
		{"name": "To", "value": m.to},
		{"name": "Date", "value": m.dateHdr},
		{"name": "Message-ID", "value": "<" + m.id + "@mail.test>"},
	}
	payload := map[string]any{"mimeType": "text/plain", "headers": headers, "body": map[string]any{"size": len(m.raw)}}
	if m.hasAttach {
		payload["parts"] = []map[string]any{
			{"mimeType": "text/plain", "filename": "", "body": map[string]any{"size": 3}},
			{"mimeType": "application/pdf", "filename": "a.pdf", "body": map[string]any{"size": 10}},
		}
	}
	return map[string]any{
		"id": m.id, "threadId": m.threadID, "labelIds": labelIDs,
		"internalDate": strconv.FormatInt(m.internalDate, 10),
		"historyId":    strconv.FormatInt(f.historySeq, 10),
		"sizeEstimate": len(m.raw),
		"payload":      payload,
	}
}

func (f *fakeGmail) getMessage(w http.ResponseWriter, id, format string) {
	f.mu.Lock()
	m, ok := f.msgs[id]
	f.mu.Unlock()
	if !ok {
		writeFakeErr(w, 404, "notFound", "Requested entity was not found.")
		return
	}
	if format == "raw" {
		writeFakeJSON(w, 200, map[string]any{
			"id": m.id, "threadId": m.threadID,
			"raw": base64.RawURLEncoding.EncodeToString([]byte(m.raw)),
		})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	writeFakeJSON(w, 200, f.metaJSON(m))
}

func (f *fakeGmail) modifyMessage(w http.ResponseWriter, id, body string) {
	var req struct {
		Add    []string `json:"addLabelIds"`
		Remove []string `json:"removeLabelIds"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	f.mu.Lock()
	m, ok := f.msgs[id]
	if !ok {
		f.mu.Unlock()
		writeFakeErr(w, 404, "notFound", "Requested entity was not found.")
		return
	}
	for _, l := range req.Add {
		m.labels[l] = true
	}
	for _, l := range req.Remove {
		delete(m.labels, l)
	}
	out := f.metaJSON(m)
	f.mu.Unlock()
	writeFakeJSON(w, 200, out)
}

func (f *fakeGmail) deleteMessage(w http.ResponseWriter, id string) {
	f.mu.Lock()
	_, ok := f.msgs[id]
	if ok {
		delete(f.msgs, id)
	}
	f.mu.Unlock()
	if !ok {
		writeFakeErr(w, 404, "notFound", "Requested entity was not found.")
		return
	}
	w.WriteHeader(204)
}

func (f *fakeGmail) sendMessage(w http.ResponseWriter, body string) {
	var req struct {
		Raw string `json:"raw"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	raw, err := base64.RawURLEncoding.DecodeString(req.Raw)
	if err != nil {
		writeFakeErr(w, 400, "invalidArgument", "bad raw")
		return
	}
	f.mu.Lock()
	id := fmt.Sprintf("sent%d", len(f.msgs)+1)
	f.msgs[id] = &fakeMsg{
		id: id, threadID: "th-" + id,
		labels:  map[string]bool{"SENT": true},
		subject: "sent subject", from: f.profileEmail, to: "rcpt@x.test",
		dateHdr: "Tue, 27 Oct 2026 10:00:00 +0800", internalDate: 1793000000000,
		raw: string(raw),
	}
	f.mu.Unlock()
	writeFakeJSON(w, 200, map[string]any{"id": id, "threadId": "th-" + id, "labelIds": []string{"SENT"}})
}

func (f *fakeGmail) createDraft(w http.ResponseWriter, body string) {
	var req struct {
		Message struct {
			Raw string `json:"raw"`
		} `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	raw, err := base64.RawURLEncoding.DecodeString(req.Message.Raw)
	if err != nil {
		writeFakeErr(w, 400, "invalidArgument", "bad raw")
		return
	}
	f.mu.Lock()
	id := fmt.Sprintf("draft%d", len(f.msgs)+1)
	f.msgs[id] = &fakeMsg{
		id: id, threadID: "th-" + id,
		labels:  map[string]bool{"DRAFT": true},
		subject: "draft subject", from: f.profileEmail, to: "rcpt@x.test",
		dateHdr: "Tue, 27 Oct 2026 10:00:00 +0800", internalDate: 1793000000000,
		raw: string(raw),
	}
	f.mu.Unlock()
	writeFakeJSON(w, 200, map[string]any{"id": "dr-" + id, "message": map[string]any{"id": id, "threadId": "th-" + id, "labelIds": []string{"DRAFT"}}})
}

func (f *fakeGmail) listHistory(w http.ResponseWriter, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	start, _ := strconv.ParseInt(get("startHistoryId"), 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	if start < f.historyValidID {
		writeFakeErr(w, 404, "notFound", "History not found.")
		return
	}
	var recs []map[string]any
	for _, rec := range f.records {
		if rec.id <= start {
			continue
		}
		r := map[string]any{"id": strconv.FormatInt(rec.id, 10)}
		msgRef := func(id string) map[string]any { return map[string]any{"id": id, "threadId": "th-" + id} }
		if len(rec.added) > 0 {
			var arr []map[string]any
			for _, id := range rec.added {
				arr = append(arr, map[string]any{"message": msgRef(id)})
			}
			r["messagesAdded"] = arr
		}
		if len(rec.deleted) > 0 {
			var arr []map[string]any
			for _, id := range rec.deleted {
				arr = append(arr, map[string]any{"message": msgRef(id)})
			}
			r["messagesDeleted"] = arr
		}
		if len(rec.labelsAdded) > 0 {
			var arr []map[string]any
			for _, c := range rec.labelsAdded {
				arr = append(arr, map[string]any{"message": msgRef(c.msgID), "labelIds": c.labels})
			}
			r["labelsAdded"] = arr
		}
		if len(rec.labelsRemoved) > 0 {
			var arr []map[string]any
			for _, c := range rec.labelsRemoved {
				arr = append(arr, map[string]any{"message": msgRef(c.msgID), "labelIds": c.labels})
			}
			r["labelsRemoved"] = arr
		}
		recs = append(recs, r)
	}
	out := map[string]any{"historyId": strconv.FormatInt(f.historySeq, 10)}
	if recs != nil {
		out["history"] = recs
	}
	writeFakeJSON(w, 200, out)
}

func (f *fakeGmail) addMessage(m *fakeMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.threadID == "" {
		m.threadID = "th-" + m.id
	}
	if m.labels == nil {
		m.labels = map[string]bool{}
	}
	f.msgs[m.id] = m
	f.historySeq++
	f.records = append(f.records, fakeHistRecord{id: f.historySeq, added: []string{m.id}})
}

func (f *fakeGmail) changeLabels(id string, add, remove []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.msgs[id]
	for _, l := range add {
		m.labels[l] = true
	}
	for _, l := range remove {
		delete(m.labels, l)
	}
	f.historySeq++
	rec := fakeHistRecord{id: f.historySeq}
	if len(add) > 0 {
		rec.labelsAdded = []fakeHistChange{{msgID: id, labels: add}}
	}
	if len(remove) > 0 {
		rec.labelsRemoved = []fakeHistChange{{msgID: id, labels: remove}}
	}
	f.records = append(f.records, rec)
}

func (f *fakeGmail) deleteMsg(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.msgs, id)
	f.historySeq++
	f.records = append(f.records, fakeHistRecord{id: f.historySeq, deleted: []string{id}})
}

func (f *fakeGmail) expireHistory() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyValidID = f.historySeq + 1
}

func (f *fakeGmail) callsMatching(substr string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if strings.Contains(c.Path, substr) || strings.Contains(c.Method+" "+c.Path, substr) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGmail) countCalls(substr string) int {
	return len(f.callsMatching(substr))
}

func (f *fakeGmail) tokenCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func stdMsg(id string, labels ...string) *fakeMsg {
	m := &fakeMsg{
		id: id, subject: "Subject " + id, from: "Alice <alice@x.test>", to: "user@gmail.com",
		dateHdr: "Mon, 26 Oct 2026 09:30:00 +0800", internalDate: 1792900000000,
		raw:    "Subject: Subject " + id + "\r\n\r\nbody of " + id,
		labels: map[string]bool{},
	}
	for _, l := range labels {
		m.labels[l] = true
	}
	return m
}
