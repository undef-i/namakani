package gmail

import (
	"reflect"
	"testing"
)

func TestMoveDeltaArchiveFromSentKeepsSentLabel(t *testing.T) {
	sent := folderDef{name: "Sent", special: "sent", labelID: "SENT"}
	inbox := folderDef{name: "INBOX", special: "inbox", labelID: "INBOX"}
	trash := folderDef{name: "Trash", special: "trash", labelID: "TRASH"}
	userLabel := folderDef{name: "Work", labelID: "Label_1"}
	allMail := folderDef{name: allMailName, special: "all"}
	cases := []struct {
		name       string
		src, dst   folderDef
		labels     []string
		wantAdd    []string
		wantRemove []string
	}{
		{"archive from sent", sent, allMail, []string{"SENT"}, nil, nil},
		{"archive from sent with inbox copy", sent, allMail, []string{"SENT", "INBOX"}, nil, []string{"INBOX"}},
		{"archive from inbox", inbox, allMail, []string{"INBOX"}, nil, []string{"INBOX"}},
		{"archive from inbox keeps starred", inbox, allMail, []string{"INBOX", "STARRED"}, nil, []string{"INBOX"}},
		{"restore from trash", trash, allMail, []string{"TRASH"}, nil, []string{"TRASH"}},
		{"archive from user label unchanged", userLabel, allMail, []string{"Label_1"}, nil, []string{"Label_1"}},
		{"move sent to trash unchanged", sent, trash, []string{"SENT"}, []string{"TRASH"}, []string{"SENT"}},
	}
	for _, c := range cases {
		add, remove := moveDelta(c.src, c.dst, &MessageMeta{LabelIDs: c.labels})
		if !reflect.DeepEqual(add, c.wantAdd) || !reflect.DeepEqual(remove, c.wantRemove) {
			t.Errorf("%s: moveDelta = add %v remove %v, want add %v remove %v",
				c.name, add, remove, c.wantAdd, c.wantRemove)
		}
	}
}
