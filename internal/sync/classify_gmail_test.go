package sync

import (
	"testing"

	"namakani/internal/gmail"
	"namakani/internal/mail"
)

func TestGmailQuotaErrorDoesNotParkRepair(t *testing.T) {
	acct := mail.Account{ID: 1, Kind: "gmail"}
	err := &gmail.APIError{Status: 403, Reason: "rateLimitExceeded", Message: "Quota exceeded for quota metric 'Total Query Cost'"}
	if term, _ := classifySessionError(acct, err); term {
		t.Fatal("a gmail quota error parked the account in repair; it must stay retryable")
	}
	authErr := &gmail.APIError{Status: 401, Reason: "unauthorized", Message: "bad token"}
	if term, _ := classifySessionError(acct, authErr); !term {
		t.Fatal("a gmail 401 no longer parks the account in repair")
	}
}
