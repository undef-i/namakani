package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Profile struct {
	EmailAddress  string `json:"emailAddress"`
	MessagesTotal int64  `json:"messagesTotal"`
	HistoryID     string `json:"historyId"`
}

func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/profile", nil, nil)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (c *Client) ListLabels(ctx context.Context) ([]Label, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/labels", nil, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Labels []Label `json:"labels"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Labels, nil
}

type MessageMeta struct {
	ID           string
	ThreadID     string
	LabelIDs     []string
	HistoryID    string
	InternalDate int64
	Size         int64
	Subject      string
	From         string
	To           string
	Cc           string
	MessageID    string
	InReplyTo    string
	References   []string
	DateHeader   string
	HasAttach    bool
}

func (m *MessageMeta) HasLabel(label string) bool {
	for _, l := range m.LabelIDs {
		if l == label {
			return true
		}
	}
	return false
}

type rawMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	InternalDate string   `json:"internalDate"`
	HistoryID    string   `json:"historyId"`
	SizeEstimate int64    `json:"sizeEstimate"`
	Payload      struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
		Parts []rawPart `json:"parts"`
	} `json:"payload"`
}

type rawPart struct {
	MimeType string    `json:"mimeType"`
	Filename string    `json:"filename"`
	Parts    []rawPart `json:"parts"`
}

func partHasAttach(p rawPart) bool {
	if p.Filename != "" {
		return true
	}
	for _, sub := range p.Parts {
		if partHasAttach(sub) {
			return true
		}
	}
	return false
}

func (r *rawMessage) meta() *MessageMeta {
	m := &MessageMeta{
		ID: r.ID, ThreadID: r.ThreadID, LabelIDs: r.LabelIDs, HistoryID: r.HistoryID,
		Size: r.SizeEstimate,
	}
	m.InternalDate, _ = strconv.ParseInt(r.InternalDate, 10, 64)
	for _, h := range r.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "subject":
			m.Subject = h.Value
		case "from":
			m.From = h.Value
		case "to":
			m.To = h.Value
		case "cc":
			m.Cc = h.Value
		case "message-id":
			m.MessageID = strings.Trim(h.Value, "<> \t")
		case "in-reply-to":
			m.InReplyTo = strings.Trim(h.Value, "<> \t")
		case "references":
			for _, tok := range strings.Fields(h.Value) {
				if id := strings.Trim(tok, "<>"); id != "" && strings.Contains(tok, "<") {
					m.References = append(m.References, id)
				}
			}
		case "date":
			m.DateHeader = h.Value
		}
	}
	for _, p := range r.Payload.Parts {
		if partHasAttach(p) {
			m.HasAttach = true
			break
		}
	}
	return m
}

func (c *Client) GetMeta(ctx context.Context, id string) (*MessageMeta, error) {
	q := url.Values{"format": {"metadata"}}
	data, _, err := c.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(id), q, nil)
	if err != nil {
		return nil, err
	}
	var rm rawMessage
	if err := json.Unmarshal(data, &rm); err != nil {
		return nil, err
	}
	return rm.meta(), nil
}

func (c *Client) GetRaw(ctx context.Context, id string) ([]byte, error) {
	q := url.Values{"format": {"raw"}}
	data, _, err := c.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(id), q, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return decodeB64URL(out.Raw)
}

// Gmail returns base64url in its canonical padded form, so strip any
// trailing padding and re-pad to a multiple of four before decoding.
func decodeB64URL(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if rem := len(s) % 4; rem != 0 {
		s += strings.Repeat("=", 4-rem)
	}
	return base64.URLEncoding.DecodeString(s)
}

func (c *Client) ListMessages(ctx context.Context, labelIDs []string, query, pageToken string) ([]string, string, error) {
	q := url.Values{"maxResults": {"500"}}
	for _, l := range labelIDs {
		q.Add("labelIds", l)
	}
	if query != "" {
		q.Set("q", query)
	}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	data, _, err := c.do(ctx, http.MethodGet, "/messages", q, nil)
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(out.Messages))
	for _, m := range out.Messages {
		ids = append(ids, m.ID)
	}
	return ids, out.NextPageToken, nil
}

func (c *Client) ListAllMessages(ctx context.Context, labelIDs []string, query string) ([]string, error) {
	var all []string
	token := ""
	for {
		ids, next, err := c.ListMessages(ctx, labelIDs, query, token)
		if err != nil {
			return nil, err
		}
		all = append(all, ids...)
		if next == "" {
			return all, nil
		}
		token = next
	}
}

type HistoryChange struct {
	MessageID string
	Labels    []string
}

type HistoryRecord struct {
	ID            string
	Added         []string
	Deleted       []string
	LabelsAdded   []HistoryChange
	LabelsRemoved []HistoryChange
}

func (c *Client) ListHistory(ctx context.Context, startHistoryID, pageToken string) ([]HistoryRecord, string, string, error) {
	q := url.Values{"startHistoryId": {startHistoryID}, "maxResults": {"500"}}
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	data, _, err := c.do(ctx, http.MethodGet, "/history", q, nil)
	if err != nil {
		return nil, "", "", err
	}
	type change struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		LabelIDs []string `json:"labelIds"`
	}
	var out struct {
		History []struct {
			ID              string   `json:"id"`
			MessagesAdded   []change `json:"messagesAdded"`
			MessagesDeleted []change `json:"messagesDeleted"`
			LabelsAdded     []change `json:"labelsAdded"`
			LabelsRemoved   []change `json:"labelsRemoved"`
		} `json:"history"`
		HistoryID     string `json:"historyId"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, "", "", err
	}
	records := make([]HistoryRecord, 0, len(out.History))
	for _, h := range out.History {
		rec := HistoryRecord{ID: h.ID}
		for _, ch := range h.MessagesAdded {
			rec.Added = append(rec.Added, ch.Message.ID)
		}
		for _, ch := range h.MessagesDeleted {
			rec.Deleted = append(rec.Deleted, ch.Message.ID)
		}
		for _, ch := range h.LabelsAdded {
			rec.LabelsAdded = append(rec.LabelsAdded, HistoryChange{MessageID: ch.Message.ID, Labels: ch.LabelIDs})
		}
		for _, ch := range h.LabelsRemoved {
			rec.LabelsRemoved = append(rec.LabelsRemoved, HistoryChange{MessageID: ch.Message.ID, Labels: ch.LabelIDs})
		}
		records = append(records, rec)
	}
	return records, out.HistoryID, out.NextPageToken, nil
}

func (c *Client) Modify(ctx context.Context, id string, add, remove []string) (*MessageMeta, error) {
	body := map[string]any{"addLabelIds": add, "removeLabelIds": remove}
	data, _, err := c.do(ctx, http.MethodPost, "/messages/"+url.PathEscape(id)+"/modify", nil, body)
	if err != nil {
		return nil, err
	}
	var rm rawMessage
	if err := json.Unmarshal(data, &rm); err != nil {
		return nil, err
	}
	return rm.meta(), nil
}

func (c *Client) Delete(ctx context.Context, id string) error {
	_, _, err := c.do(ctx, http.MethodDelete, "/messages/"+url.PathEscape(id), nil, nil)
	return err
}

func (c *Client) Send(ctx context.Context, raw []byte) (string, error) {
	body := map[string]any{"raw": base64.RawURLEncoding.EncodeToString(raw)}
	data, _, err := c.do(ctx, http.MethodPost, "/messages/send", nil, body)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *Client) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	body := map[string]any{"message": map[string]any{"raw": base64.RawURLEncoding.EncodeToString(raw)}}
	data, _, err := c.do(ctx, http.MethodPost, "/drafts", nil, body)
	if err != nil {
		return "", err
	}
	var out struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	return out.Message.ID, nil
}
