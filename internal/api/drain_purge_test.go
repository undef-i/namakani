package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPurgeFolderEnqueuesExpunges(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 3, "Trash", 100, 0)
	f.addMessage(t, 11, 3, "Trash", 101, 0)
	f.addMessage(t, 12, 3, "Trash", 102, 0)
	req := httptest.NewRequest(http.MethodPost, "/api/folders/3/purge", nil)
	req.SetPathValue("id", "3")
	rec := httptest.NewRecorder()
	f.srv.handlePurgeFolder(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("purge status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Deleted != 3 {
		t.Fatalf("purge deleted = %d (err %v), want 3", resp.Deleted, err)
	}
	hidden := f.st.PendingExpungeSet([]int64{10, 11, 12})
	for _, id := range []int64{10, 11, 12} {
		if !hidden[id] {
			t.Fatalf("message %d is still visible right after purge", id)
		}
	}
	var total int
	_ = f.st.DB.QueryRow("SELECT total FROM folders WHERE id = 3").Scan(&total)
	if total != 0 {
		t.Fatalf("Trash total right after purge = %d, want 0", total)
	}
	f.srv.drainAccount(nil, 1)
	for _, id := range []int64{10, 11, 12} {
		if _, err := f.st.MessageRoute(id); err == nil {
			t.Fatalf("local row %d survived the purge drain", id)
		}
	}
	if got := len(f.p.folders["Trash"]); got != 0 {
		t.Fatalf("provider Trash still holds %d messages after the purge drain", got)
	}
}
