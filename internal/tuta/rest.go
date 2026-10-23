package tuta

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type instance map[string]any

func (in instance) str(key string) string {
	if v, ok := in[key].(string); ok {
		return v
	}
	return ""
}

func (in instance) num(key string) int64 {
	s := in.str(key)
	if s == "" {
		return 0
	}
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}

func (in instance) boolean(key string) bool {
	return in.str(key) == "1"
}

func (in instance) bytes(key string) []byte {
	s := in.str(key)
	if s == "" {
		return nil
	}
	b, err := b64decode(s)
	if err != nil {
		return nil
	}
	return b
}

func (in instance) agg(key string) instance {
	if v, ok := in[key].(map[string]any); ok {
		return instance(v)
	}
	if v, ok := in[key].([]any); ok {
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				return instance(m)
			}
		}
	}
	return nil
}

func (in instance) aggList(key string) []instance {
	v, ok := in[key].([]any)
	if !ok {
		return nil
	}
	out := make([]instance, 0, len(v))
	for _, item := range v {
		if m, ok := item.(map[string]any); ok {
			out = append(out, instance(m))
		}
	}
	return out
}

func (in instance) idList(key string) []string {
	v, ok := in[key].([]any)
	if !ok {
		if s := in.str(key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(v))
	for _, item := range v {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (in instance) assocID(key string) string {
	ids := in.idList(key)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (in instance) elemID(key string) string {
	if _, el, ok := in.idTuple(key); ok {
		return el
	}
	return in.str(key)
}

func (in instance) idTuple(key string) (string, string, bool) {
	v, ok := in[key].([]any)
	if !ok || len(v) != 2 {
		return "", "", false
	}
	l, lok := v[0].(string)
	e, eok := v[1].(string)
	return l, e, lok && eok
}

func (in instance) idTuples(key string) [][2]string {
	v, ok := in[key].([]any)
	if !ok {
		return nil
	}
	out := make([][2]string, 0, len(v))
	for _, item := range v {
		if pair, ok := item.([]any); ok && len(pair) == 2 {
			l, lok := pair[0].(string)
			e, eok := pair[1].(string)
			if lok && eok {
				out = append(out, [2]string{l, e})
			}
		}
	}
	return out
}

func b64encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func b64decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

const (
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 20 * time.Second
)

type restClient struct {
	http        *http.Client
	accessToken string
	ctx         context.Context
}

func newRestClient() *restClient {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConnsPerHost:   4,
	}
	return &restClient{http: &http.Client{Transport: transport}}
}

type restRequest struct {
	method      string
	path        string
	version     string
	baseURL     string
	query       url.Values
	body        []byte
	contentType string
	binary      bool
	extraHeader map[string]string
}

func (c *restClient) do(req restRequest) ([]byte, int, error) {
	base := req.baseURL
	if base == "" {
		base = apiBaseURL
	}
	u := base + req.path
	if len(req.query) > 0 {
		u += "?" + req.query.Encode()
	}
	var bodyReader io.Reader
	if req.body != nil {
		bodyReader = bytes.NewReader(req.body)
	}
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, u, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	if c.accessToken != "" {
		httpReq.Header.Set("accessToken", c.accessToken)
	}
	if req.version != "" {
		httpReq.Header.Set("v", req.version)
	}
	httpReq.Header.Set("cv", clientVersion)
	httpReq.Header.Set("cp", clientPlatformWeb)
	if req.contentType != "" {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	for k, v := range req.extraHeader {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := parseRestError(resp.StatusCode, data)
		if re, ok := err.(*RestError); ok {
			if eid := resp.Header.Get("error-id"); eid != "" {
				if re.Message == "" {
					re.Message = "error-id: " + eid
				} else {
					re.Message += " (error-id: " + eid + ")"
				}
			}
		}
		return data, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

func parseRestError(status int, data []byte) error {
	re := &RestError{StatusCode: status}
	var body struct {
		ErrorMessage string `json:"errorMessage"`
		Message      string `json:"message"`
		Name         string `json:"name"`
	}
	if err := json.Unmarshal(data, &body); err == nil {
		re.Message = body.ErrorMessage
		if re.Message == "" {
			re.Message = body.Message
		}
		re.Name = body.Name
	}
	if re.Message == "" {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		re.Message = msg
	}
	return re
}

func decodeInstances(data []byte) ([]instance, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]instance, len(raw))
	for i, r := range raw {
		out[i] = instance(r)
	}
	return out, nil
}

func decodeInstance(data []byte) (instance, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return instance(raw), nil
}

func marshalInstance(in instance) ([]byte, error) {
	return json.Marshal(map[string]any(in))
}

func entityPath(app, typeName string, ids ...string) string {
	var b strings.Builder
	b.WriteString("/rest/")
	b.WriteString(app)
	b.WriteString("/")
	b.WriteString(typeName)
	for _, id := range ids {
		b.WriteString("/")
		b.WriteString(url.PathEscape(id))
	}
	return b.String()
}

func servicePath(app, serviceName string) string {
	return "/rest/" + app + "/" + strings.ToLower(serviceName)
}

func modelVersion(app string) string {
	switch app {
	case "sys":
		return modelVersionSys
	case "tutanota":
		return modelVersionTutanota
	case "storage":
		return modelVersionStorage
	}
	return ""
}

func (c *restClient) getEntity(app, typeName string, version string, ids ...string) (instance, error) {
	data, _, err := c.do(restRequest{
		method:  http.MethodGet,
		path:    entityPath(app, typeName, ids...),
		version: version,
	})
	if err != nil {
		return nil, err
	}
	return decodeInstance(data)
}

func (c *restClient) loadRange(app, typeName, version, listID, start string, count int, reverse bool) ([]instance, error) {
	q := url.Values{}
	q.Set("start", start)
	q.Set("count", fmt.Sprint(count))
	q.Set("reverse", fmt.Sprint(reverse))
	data, _, err := c.do(restRequest{
		method:  http.MethodGet,
		path:    entityPath(app, typeName, listID),
		version: version,
		query:   q,
	})
	if err != nil {
		return nil, err
	}
	return decodeInstances(data)
}

func (c *restClient) loadMultiple(app, typeName, version, listID string, ids []string) ([]instance, error) {
	q := url.Values{}
	q.Set("ids", strings.Join(ids, ","))
	path := entityPath(app, typeName)
	if listID != "" {
		path = entityPath(app, typeName, listID)
	}
	data, _, err := c.do(restRequest{
		method:  http.MethodGet,
		path:    path,
		version: version,
		query:   q,
	})
	if err != nil {
		return nil, err
	}
	return decodeInstances(data)
}

func (c *restClient) setupEntity(app, typeName, version string, in instance) (string, error) {
	body, err := marshalInstance(in)
	if err != nil {
		return "", err
	}
	data, _, err := c.do(restRequest{
		method:      http.MethodPost,
		path:        entityPath(app, typeName),
		version:     version,
		body:        body,
		contentType: "application/json",
	})
	if err != nil {
		return "", err
	}
	return strings.Trim(string(data), `"`), nil
}

func (c *restClient) updateEntity(app, typeName, version string, ids []string, in instance) error {
	body, err := marshalInstance(in)
	if err != nil {
		return err
	}
	_, _, err = c.do(restRequest{
		method:      http.MethodPut,
		path:        entityPath(app, typeName, ids...),
		version:     version,
		body:        body,
		contentType: "application/json",
	})
	return err
}

func (c *restClient) eraseEntity(app, typeName, version string, ids ...string) error {
	_, _, err := c.do(restRequest{
		method:  http.MethodDelete,
		path:    entityPath(app, typeName, ids...),
		version: version,
	})
	return err
}

func (c *restClient) callService(method, app, serviceName, version string, in instance) (instance, error) {
	var body []byte
	if in != nil {
		var err error
		body, err = marshalInstance(in)
		if err != nil {
			return nil, err
		}
	}
	data, _, err := c.do(restRequest{
		method:      method,
		path:        servicePath(app, serviceName),
		version:     version,
		body:        body,
		contentType: "application/json",
	})
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	return decodeInstance(data)
}
