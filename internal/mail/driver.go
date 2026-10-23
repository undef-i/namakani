package mail

import (
	"context"

	"github.com/emersion/go-imap/v2"
)

type BackgroundTurner interface {
	BackgroundTurn(ctx context.Context) (func(), bool)
}

type BodyPrefetcher interface {
	PrefetchBodies(uids []uint32) error
}

type UIDLister interface {
	FetchUIDs() ([]uint32, error)
}

type SelectData struct {
	UIDValidity uint32
	UIDNext     uint32
	NumMessages uint32
}

var _ Conn = (*Client)(nil)

type Conn interface {
	Close()
	Capabilities() []string
	HasCap(cap imap.Cap) bool
	ListFolders() ([]FolderInfo, error)
	Select(name string) (*SelectData, error)
	SelectRW(name string) (*SelectData, error)
	FetchEnvelopes(uids []uint32) ([]Msg, error)
	FetchFlags(changedSince uint64) ([]Msg, error)
	FetchRaw(uid uint32) ([]byte, error)
	FetchThreadHeaders(uids []uint32) (map[uint32]ThreadHeaders, error)
	StoreFlag(uid uint32, flag imap.Flag, add bool) error
	StoreFlags(uids []uint32, flag imap.Flag, add bool) error
	MoveMessage(uid uint32, dest string) (uint32, error)
	DeleteMessage(uid uint32) error
	AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error)
}
