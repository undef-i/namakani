package tuta

import (
	"bytes"
	"testing"
)

func mustDecode(t *testing.T, js string) instance {
	t.Helper()
	in, err := decodeInstance([]byte(js))
	if err != nil {
		t.Fatalf("decodeInstance: %v", err)
	}
	return in
}

func TestSessionReturnUserAssoc(t *testing.T) {
	in := mustDecode(t, `{"1220":"0","1221":"access-token-1","1222":[],"1223":["NUserId123"]}`)
	if got := in.str("1221"); got != "access-token-1" {
		t.Fatalf("accessToken = %q", got)
	}
	if got := in.assocID("1223"); got != "NUserId123" {
		t.Fatalf("user assocID = %q, want NUserId123", got)
	}
	if got := in.str("1223"); got != "" {
		t.Fatalf("user str = %q, want empty (wire shape is an id array)", got)
	}
}

func TestGroupMembershipGroupAssoc(t *testing.T) {
	in := mustDecode(t, `{"26":"membershipId","27":"AAE=","29":["groupId42"],"2246":"3","2247":"3","1030":"0"}`)
	if got := in.assocID("29"); got != "groupId42" {
		t.Fatalf("group assocID = %q, want groupId42", got)
	}
}

func TestUserGroupAggregateMembership(t *testing.T) {
	in := mustDecode(t, `{"95":[{"26":"m1","29":["userGroupId"]}],"96":[{"26":"m2","29":["mailGroupId"]}]}`)
	ug := in.agg("95")
	if ug == nil {
		t.Fatal("userGroup aggregate missing")
	}
	if got := ug.assocID("29"); got != "userGroupId" {
		t.Fatalf("userGroup group = %q, want userGroupId", got)
	}
	mems := in.aggList("96")
	if len(mems) != 1 || mems[0].assocID("29") != "mailGroupId" {
		t.Fatalf("memberships = %v", mems)
	}
}

func TestMailboxGroupRootMailboxAssoc(t *testing.T) {
	in := mustDecode(t, `{"695":"rootId","699":["mailboxId7"]}`)
	if got := in.assocID("699"); got != "mailboxId7" {
		t.Fatalf("mailbox assocID = %q, want mailboxId7", got)
	}
}

func TestBucketKeyGroupAssoc(t *testing.T) {
	in := mustDecode(t, `{"2044":"bkId","2047":["keyGroup9"],"2048":[]}`)
	if got := in.assocID("2047"); got != "keyGroup9" {
		t.Fatalf("keyGroup assocID = %q, want keyGroup9", got)
	}
	empty := mustDecode(t, `{"2044":"bkId","2047":[]}`)
	if got := empty.assocID("2047"); got != "" {
		t.Fatalf("empty keyGroup assocID = %q, want empty", got)
	}
}

func TestListElementIDs(t *testing.T) {
	entry := mustDecode(t, `{"1452":["setListId","entryElemId"],"1456":["mailListId","mailElemId"]}`)
	if got := entry.elemID("1452"); got != "entryElemId" {
		t.Fatalf("entry elemID = %q, want entryElemId", got)
	}
	if l, el, ok := entry.idTuple("1456"); !ok || l != "mailListId" || el != "mailElemId" {
		t.Fatalf("entry mail tuple = %q %q %v", l, el, ok)
	}
	set := mustDecode(t, `{"431":["folderListId","setElemId"],"436":"1"}`)
	if got := set.elemID("431"); got != "setElemId" {
		t.Fatalf("set elemID = %q, want setElemId", got)
	}
	gk := mustDecode(t, `{"2257":["keysListId","customVersionId"],"2261":"AAE="}`)
	if got := gk.elemID("2257"); got != "customVersionId" {
		t.Fatalf("groupkey elemID = %q, want customVersionId", got)
	}
}

func TestUserMembershipsChain(t *testing.T) {
	userGroupMembership := `{"26":"membershipCustomId1","27":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4v","28":"1","2246":"1","2247":"1","29":["NUserGroupId1"],"30":[["groupInfoList","groupInfoElem"]],"230":[["groupMemberList","groupMemberElem"]]}`
	mailMembership := `{"26":"membershipCustomId2","27":"AAECAwQFBgcICQoLDA0ODw==","28":"0","1030":"5","2246":"0","2247":"0","29":["NMailGroupId1"],"30":[["groupInfoList","groupInfoElem2"]],"230":[["groupMemberList","groupMemberElem2"]]}`
	user := mustDecode(t, `{"86":"NUserId1","88":"0","92":"1","93":"1","996":"NUserGroupId1","95":[`+userGroupMembership+`],"96":[`+mailMembership+`]}`)
	mems, userGroupID := userMemberships(user)
	if userGroupID != "NUserGroupId1" {
		t.Fatalf("userGroupID = %q, want NUserGroupId1", userGroupID)
	}
	if len(mems) != 2 {
		t.Fatalf("memberships = %d, want 2", len(mems))
	}
	wantKey := make([]byte, 48)
	for i := range wantKey {
		wantKey[i] = byte(i)
	}
	found := 0
	for _, m := range mems {
		if m.groupID == userGroupID {
			found++
			if !bytes.Equal(m.symEncGKey, wantKey) {
				t.Fatalf("user group symEncGKey mismatch: %d bytes", len(m.symEncGKey))
			}
			if m.version != 1 || m.symKeyVer != 1 {
				t.Fatalf("user group versions = %d/%d, want 1/1", m.version, m.symKeyVer)
			}
		}
	}
	if found != 1 {
		t.Fatalf("memberships matching user group = %d, want 1", found)
	}
}
