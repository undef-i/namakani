package tuta

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

func genAggID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

const maxBlobSizeBytes = 10 * 1024 * 1024

type blobAccessInfo struct {
	token   string
	servers []string
	expires time.Time
}

type tokenCache struct {
	mu       sync.Mutex
	archives map[string]*blobAccessInfo
	writes   map[string]*blobAccessInfo
}

func (s *Session) tokens() *tokenCache {
	return s.tokenCache
}

func (s *Session) requestBlobToken(in instance) (*blobAccessInfo, error) {
	out, err := s.rest.callService(http.MethodPost, "storage", "blobaccesstokenservice", modelVersionStorage, in)
	if err != nil {
		return nil, err
	}
	info := out.agg("161")
	if info == nil {
		return nil, errors.New("tuta: blob access token response missing info")
	}
	res := &blobAccessInfo{token: info.str("159")}
	for _, srv := range info.aggList("160") {
		if u := srv.str("156"); u != "" {
			res.servers = append(res.servers, u)
		}
	}
	if exp := info.num("192"); exp > 0 {
		res.expires = time.UnixMilli(exp)
	}
	if len(res.servers) == 0 {
		return nil, errors.New("tuta: blob access token response has no servers")
	}
	return res, nil
}

func archiveTokenRequest(archiveID string) instance {
	return instance{
		"78":  "0",
		"180": nil,
		"80":  []any{},
		"181": []any{map[string]any{
			"176": genAggID(),
			"177": archiveID,
			"178": nil,
			"179": []any{},
		}},
	}
}

func writeTokenRequest(ownerGroup string) instance {
	return instance{
		"78":  "0",
		"180": "1",
		"181": []any{},
		"80": []any{map[string]any{
			"74": genAggID(),
			"75": ownerGroup,
		}},
	}
}

func (s *Session) archiveToken(archiveID string) (*blobAccessInfo, error) {
	tc := s.tokens()
	tc.mu.Lock()
	if info, ok := tc.archives[archiveID]; ok && time.Now().Before(info.expires) {
		tc.mu.Unlock()
		return info, nil
	}
	tc.mu.Unlock()
	info, err := s.requestBlobToken(archiveTokenRequest(archiveID))
	if err != nil {
		return nil, err
	}
	tc.mu.Lock()
	tc.archives[archiveID] = info
	tc.mu.Unlock()
	return info, nil
}

func (s *Session) writeToken(ownerGroup string) (*blobAccessInfo, error) {
	tc := s.tokens()
	tc.mu.Lock()
	if info, ok := tc.writes[ownerGroup]; ok && time.Now().Before(info.expires) {
		tc.mu.Unlock()
		return info, nil
	}
	tc.mu.Unlock()
	info, err := s.requestBlobToken(writeTokenRequest(ownerGroup))
	if err != nil {
		return nil, err
	}
	tc.mu.Lock()
	tc.writes[ownerGroup] = info
	tc.mu.Unlock()
	return info, nil
}

func (s *Session) blobQuery(info *blobAccessInfo, version string, extra url.Values) url.Values {
	q := url.Values{}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("blobAccessToken", info.token)
	q.Set("v", version)
	if s.accessToken != "" {
		q.Set("accessToken", s.accessToken)
	}
	return q
}

func (s *Session) loadBlobElements(archiveID string, elementIDs []string) ([]instance, error) {
	info, err := s.archiveToken(archiveID)
	if err != nil {
		return nil, err
	}
	ids := ""
	for i, id := range elementIDs {
		if i > 0 {
			ids += ","
		}
		ids += id
	}
	q := s.blobQuery(info, modelVersionTutanota, url.Values{"ids": {ids}})
	var lastErr error
	for _, server := range info.servers {
		data, _, err := s.rest.do(restRequest{
			method:  http.MethodGet,
			baseURL: server,
			path:    "/rest/tutanota/maildetailsblob/" + url.PathEscape(archiveID),
			query:   q,
		})
		if err != nil {
			lastErr = err
			continue
		}
		return decodeInstances(data)
	}
	return nil, lastErr
}

func (s *Session) downloadBlobs(archiveID string, blobIDs []string) (map[string][]byte, error) {
	info, err := s.archiveToken(archiveID)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte)
	for start := 0; start < len(blobIDs); start += 100 {
		end := start + 100
		if end > len(blobIDs) {
			end = len(blobIDs)
		}
		chunk, err := s.downloadBlobChunk(info, archiveID, blobIDs[start:end])
		if err != nil {
			return nil, err
		}
		for k, v := range chunk {
			out[k] = v
		}
	}
	return out, nil
}

func blobGetInRequest(archiveID string, blobIDs []string) instance {
	ids := make([]any, 0, len(blobIDs))
	for _, id := range blobIDs {
		ids = append(ids, map[string]any{"145": genAggID(), "146": id})
	}
	return instance{
		"51":  "0",
		"52":  archiveID,
		"110": nil,
		"193": ids,
	}
}

func (s *Session) downloadBlobChunk(info *blobAccessInfo, archiveID string, blobIDs []string) (map[string][]byte, error) {
	body, err := marshalInstance(blobGetInRequest(archiveID, blobIDs))
	if err != nil {
		return nil, err
	}
	q := s.blobQuery(info, modelVersionStorage, nil)
	var lastErr error
	for _, server := range info.servers {
		data, _, err := s.rest.do(restRequest{
			method:      http.MethodGet,
			baseURL:     server,
			path:        "/rest/storage/blobservice",
			query:       q,
			body:        body,
			contentType: "application/json",
		})
		if err != nil {
			lastErr = err
			continue
		}
		return parseMultipleBlobsResponse(data)
	}
	return nil, lastErr
}

func parseMultipleBlobsResponse(data []byte) (map[string][]byte, error) {
	if len(data) < 4 {
		return nil, errors.New("tuta: blob response too short")
	}
	count := int(binary.BigEndian.Uint32(data[0:4]))
	if count < 0 {
		return nil, fmt.Errorf("tuta: invalid blob count %d", count)
	}
	out := make(map[string][]byte, count)
	offset := 4
	for offset < len(data) {
		if offset+19 > len(data) {
			return nil, errors.New("tuta: blob response truncated")
		}
		blobID := blobIDString(data[offset : offset+9])
		size := int(binary.BigEndian.Uint32(data[offset+15 : offset+19]))
		start := offset + 19
		if size < 0 || start+size > len(data) {
			return nil, fmt.Errorf("tuta: invalid blob size %d", size)
		}
		out[blobID] = data[start : start+size]
		offset = start + size
	}
	if len(out) != count {
		return nil, fmt.Errorf("tuta: parsed %d blobs, expected %d", len(out), count)
	}
	return out, nil
}

const base64ExtAlphabet = "-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz"
const base64StdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func blobIDString(raw []byte) string {
	b64 := b64encode(raw)
	res := []byte(b64)
	for i, c := range res {
		if c == '=' {
			continue
		}
		for j := 0; j < len(base64StdAlphabet); j++ {
			if base64StdAlphabet[j] == c {
				res[i] = base64ExtAlphabet[j]
				break
			}
		}
	}
	return string(res)
}

func (s *Session) uploadBlobChunk(ownerGroup string, encrypted []byte) (string, error) {
	info, err := s.writeToken(ownerGroup)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encrypted)
	q := s.blobQuery(info, modelVersionStorage, url.Values{
		"blobHash": {b64encode(sum[:6])},
	})
	var lastErr error
	for _, server := range info.servers {
		data, _, err := s.rest.do(restRequest{
			method:      http.MethodPost,
			baseURL:     server,
			path:        "/rest/storage/blobservice",
			query:       q,
			body:        encrypted,
			contentType: "application/octet-stream",
		})
		if err != nil {
			lastErr = err
			continue
		}
		out, err := decodeInstance(data)
		if err != nil {
			return "", err
		}
		if tok := out.str("127"); tok != "" {
			return tok, nil
		}
		for _, w := range out.aggList("208") {
			if tok := w.str("1992"); tok != "" {
				return tok, nil
			}
		}
		return "", errors.New("tuta: blob upload response missing reference token")
	}
	return "", lastErr
}
