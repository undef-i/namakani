package store

import (
	"testing"
	"time"
)

func TestAccelerateMakesBackedOffOpClaimable(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	opID, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil || opID == 0 {
		t.Fatalf("enqueue: op=%d err=%v", opID, err)
	}
	if _, err := st.ClaimDueOps(1, now, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.FailOp(opID, "dial timeout", false, now); err != nil {
		t.Fatalf("fail: %v", err)
	}
	due, err := st.ClaimDueOps(1, now+1, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 0 {
		t.Fatal("backed-off op must not be claimed before acceleration")
	}
	if err := st.AccelerateAccountOps(1, now+1, nil); err != nil {
		t.Fatalf("accelerate: %v", err)
	}
	due, err = st.ClaimDueOps(1, now+1, 10)
	if err != nil {
		t.Fatalf("claim after accelerate: %v", err)
	}
	if len(due) != 1 || due[0].ID != opID {
		t.Fatalf("after accelerate claimed %v, want the backed-off op", due)
	}
}

func TestAccelerateSkipsExcludedOps(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	a, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil || a == 0 {
		t.Fatalf("enqueue a: op=%d err=%v", a, err)
	}
	b, _, err := st.EnqueueFlags(1, 11, boolPtr(true), nil, "")
	if err != nil || b == 0 {
		t.Fatalf("enqueue b: op=%d err=%v", b, err)
	}
	if _, err := st.ClaimDueOps(1, now, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.FailOp(a, "dial timeout", false, now); err != nil {
		t.Fatalf("fail a: %v", err)
	}
	if _, err := st.FailOp(b, "dial timeout", false, now); err != nil {
		t.Fatalf("fail b: %v", err)
	}
	if err := st.AccelerateAccountOps(1, now+1, []int64{a}); err != nil {
		t.Fatalf("accelerate: %v", err)
	}
	due, err := st.ClaimDueOps(1, now+1, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 1 || due[0].ID != b {
		t.Fatalf("claimed %v, want only the non-excluded op", due)
	}
}

func TestAccelerateDoesNotLiftNotBefore(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	opID, err := st.EnqueueSend(1, "{}", now+3600, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	if _, err := st.ClaimDueOps(1, now+3601, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.FailOp(opID, "dial timeout", false, now); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := st.AccelerateAccountOps(1, now+5, nil); err != nil {
		t.Fatalf("accelerate: %v", err)
	}
	due, err := st.ClaimDueOps(1, now+5, 10)
	if err != nil {
		t.Fatalf("claim inside window: %v", err)
	}
	if len(due) != 0 {
		t.Fatal("acceleration must not lift the not_before gate")
	}
	due, err = st.ClaimDueOps(1, now+3601, 10)
	if err != nil {
		t.Fatalf("claim after window: %v", err)
	}
	if len(due) != 1 || due[0].ID != opID {
		t.Fatalf("after window claimed %v, want the send op", due)
	}
}

