package sync

import (
	"path/filepath"
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func TestNotifyGmailPushRoutesByEmail(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	m := NewManager(st)
	m.workers[6] = &worker{acct: mail.Account{ID: 6, Kind: "gmail", Email: "nelklanita@gmail.com"}}
	m.workers[7] = &worker{acct: mail.Account{ID: 7, Kind: "gmail", Email: "noxylva@gmail.com"}}
	ch6 := m.registerGmailPush(6)
	ch7 := m.registerGmailPush(7)

	m.NotifyGmailPush("nelklanita@gmail.com")
	select {
	case <-ch6:
	default:
		t.Fatal("account 6 was not notified")
	}
	select {
	case <-ch7:
		t.Fatal("account 7 must not be notified for account 6 mail")
	default:
	}

	m.NotifyGmailPush("nobody@example.com")
	select {
	case <-ch6:
		t.Fatal("unknown sender address notified account 6")
	default:
	}
	select {
	case <-ch7:
		t.Fatal("unknown sender address notified account 7")
	default:
	}

	m.unregisterGmailPush(6)
	m.NotifyGmailPush("nelklanita@gmail.com")
	select {
	case <-ch6:
		t.Fatal("unregistered account 6 was notified")
	default:
	}
}
