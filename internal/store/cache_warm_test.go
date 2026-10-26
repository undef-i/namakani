package store

import "testing"

func TestWarmBodyBytesPopulatesExactTotal(t *testing.T) {
	st := opsTestStore(t)
	if err := st.SaveBody(10, "hello", "<p>world</p>", "hi", false); err != nil {
		t.Fatalf("save body 10: %v", err)
	}
	if err := st.SaveBody(11, "abc", "", "hi", false); err != nil {
		t.Fatalf("save body 11: %v", err)
	}
	st.bodyBytesMu.Lock()
	st.bodyBytesOK = false
	st.bodyBytes = 0
	st.bodyBytesMu.Unlock()
	st.WarmBodyBytes()
	want := int64(len("hello") + len("<p>world</p>") + len("abc"))
	got, err := st.cacheBodyBytes()
	if err != nil {
		t.Fatalf("cacheBodyBytes: %v", err)
	}
	if got != want {
		t.Fatalf("body bytes = %d, want %d", got, want)
	}
	st.bodyBytesMu.Lock()
	ok := st.bodyBytesOK
	st.bodyBytesMu.Unlock()
	if !ok {
		t.Fatalf("cache not populated after warm: the first status request after a restart would still pay the full scan")
	}
	if err := st.SaveBody(11, "abcdef", "", "hi", false); err != nil {
		t.Fatalf("save body 11 again: %v", err)
	}
	got, err = st.cacheBodyBytes()
	if err != nil {
		t.Fatalf("cacheBodyBytes after save: %v", err)
	}
	if got != want+3 {
		t.Fatalf("body bytes after incremental save = %d, want %d", got, want+3)
	}
}
