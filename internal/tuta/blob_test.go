package tuta

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"testing"
)

var aggIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{6}$`)

func TestGenAggIDShape(t *testing.T) {
	for i := 0; i < 64; i++ {
		id := genAggID()
		if !aggIDPattern.MatchString(id) {
			t.Fatalf("agg id %q does not match 6-char base64url without padding", id)
		}
		raw, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil || len(raw) != 4 {
			t.Fatalf("agg id %q decodes to %d bytes, err %v", id, len(raw), err)
		}
	}
}

func TestArchiveTokenRequestShape(t *testing.T) {
	body, err := marshalInstance(archiveTokenRequest("ARCHIVE1"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["78"] != "0" {
		t.Fatalf("_format = %v", got["78"])
	}
	if v, ok := got["180"]; !ok || v != nil {
		t.Fatalf("archiveDataType = %v (present=%v), want explicit null", v, ok)
	}
	if v, ok := got["80"].([]any); !ok || len(v) != 0 {
		t.Fatalf("write = %v, want empty array", got["80"])
	}
	reads, ok := got["181"].([]any)
	if !ok || len(reads) != 1 {
		t.Fatalf("read = %v, want single-element array", got["181"])
	}
	read, ok := reads[0].(map[string]any)
	if !ok {
		t.Fatalf("read[0] = %T", reads[0])
	}
	if id, ok := read["176"].(string); !ok || !aggIDPattern.MatchString(id) {
		t.Fatalf("read _id = %v, want generated aggregate id", read["176"])
	}
	if read["177"] != "ARCHIVE1" {
		t.Fatalf("archiveId = %v", read["177"])
	}
	if v, ok := read["178"]; !ok || v != nil {
		t.Fatalf("instanceListId = %v, want explicit null", v)
	}
	if v, ok := read["179"].([]any); !ok || len(v) != 0 {
		t.Fatalf("instanceIds = %v, want empty array", read["179"])
	}
}

func TestBlobGetInRequestShape(t *testing.T) {
	body, err := marshalInstance(blobGetInRequest("ARCHIVE1", []string{"BLOB1", "BLOB2"}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["51"] != "0" || got["52"] != "ARCHIVE1" {
		t.Fatalf("header fields = %v", got)
	}
	if v, ok := got["110"]; !ok || v != nil {
		t.Fatalf("blobId = %v (present=%v), want explicit null", v, ok)
	}
	ids, ok := got["193"].([]any)
	if !ok || len(ids) != 2 {
		t.Fatalf("blobIds = %v, want two entries", got["193"])
	}
	for i, want := range []string{"BLOB1", "BLOB2"} {
		entry, ok := ids[i].(map[string]any)
		if !ok {
			t.Fatalf("blobIds[%d] = %T", i, ids[i])
		}
		if id, ok := entry["145"].(string); !ok || !aggIDPattern.MatchString(id) {
			t.Fatalf("blobIds[%d] _id = %v, want generated aggregate id", i, entry["145"])
		}
		if entry["146"] != want {
			t.Fatalf("blobIds[%d] blobId = %v, want %v", i, entry["146"], want)
		}
	}
}

func TestWriteTokenRequestShape(t *testing.T) {
	body, err := marshalInstance(writeTokenRequest("GROUP1"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["180"] != "1" {
		t.Fatalf("archiveDataType = %v", got["180"])
	}
	if v, ok := got["181"].([]any); !ok || len(v) != 0 {
		t.Fatalf("read = %v, want empty array", got["181"])
	}
	writes, ok := got["80"].([]any)
	if !ok || len(writes) != 1 {
		t.Fatalf("write = %v, want single-element array", got["80"])
	}
	write, ok := writes[0].(map[string]any)
	if !ok {
		t.Fatalf("write[0] = %T", writes[0])
	}
	if id, ok := write["74"].(string); !ok || !aggIDPattern.MatchString(id) {
		t.Fatalf("write _id = %v, want generated aggregate id", write["74"])
	}
	if write["75"] != "GROUP1" {
		t.Fatalf("archiveOwnerGroup = %v", write["75"])
	}
}
