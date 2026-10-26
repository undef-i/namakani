package mail

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

const fakeDoubleBracketID = "1792852919064309523.7a0fd8b8b2b5@example.com"

func fakeIMAPServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeIMAP(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func serveFakeIMAP(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(s string) {
		w.WriteString(s + "\r\n")
		w.Flush()
	}
	send("* OK fake imap ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			continue
		}
		tag, cmd := parts[0], strings.ToUpper(parts[1])
		switch cmd {
		case "LOGIN":
			send(tag + " OK logged in")
		case "CAPABILITY":
			send("* CAPABILITY IMAP4rev1 UIDPLUS")
			send(tag + " OK capability done")
		case "SELECT", "EXAMINE":
			send(`* FLAGS (\Seen \Flagged)`)
			send("* 2 EXISTS")
			send("* 0 RECENT")
			send("* OK [UIDVALIDITY 777] uidvalidity")
			send("* OK [UIDNEXT 3] uidnext")
			send(tag + " OK [READ-ONLY] select done")
		case "FETCH":
			serveFakeFetch(send, line, tag, 0)
		case "UID":
			if len(parts) == 3 && strings.HasPrefix(strings.ToUpper(parts[2]), "FETCH") {
				serveFakeFetch(send, line, tag, 0)
			} else {
				send(tag + " OK uid done")
			}
		case "LOGOUT":
			send("* BYE bye")
			send(tag + " OK logout done")
			return
		default:
			send(tag + " OK done")
		}
	}
}

func fakeFetchLine(seq int, uid int, envMsgID, hdrMsgID string, withHeader bool) string {
	env := fmt.Sprintf(`ENVELOPE ("Sat, 25 Oct 2026 10:00:00 +0800" "fake subject %d" (("A" NIL "a" "example.com")) (("A" NIL "a" "example.com")) (("A" NIL "a" "example.com")) (("B" NIL "b" "example.com")) NIL NIL NIL "%s")`, uid, envMsgID)
	items := fmt.Sprintf(`UID %d FLAGS () INTERNALDATE "25-Oct-2026 10:00:00 +0800" RFC822.SIZE 42 %s BODYSTRUCTURE ("text" "plain" ("charset" "utf-8") NIL NIL "7bit" 5 1 NIL NIL NIL NIL)`, uid, env)
	if withHeader {
		hdr := "Message-Id: " + hdrMsgID + "\r\n"
		items += fmt.Sprintf(` BODY[HEADER.FIELDS (MESSAGE-ID)] {%d}`, len(hdr))
		return fmt.Sprintf("* %d FETCH (%s\r\n%s)", seq, items, hdr)
	}
	return fmt.Sprintf("* %d FETCH (%s)", seq, items)
}

func serveFakeFetch(send func(string), line, tag string, _ int) {
	withHeader := strings.Contains(strings.ToUpper(line), "HEADER.FIELDS")
	send(fakeFetchLine(1, 1, "<<"+fakeDoubleBracketID+">>", "<<"+fakeDoubleBracketID+">>", withHeader))
	send(fakeFetchLine(2, 2, "<normal-1@example.com>", "<normal-1@example.com>", withHeader))
	send(tag + " OK fetch done")
}

func TestFetchEnvelopesMessageIDFromRawHeaderForms(t *testing.T) {
	host, port := fakeIMAPServer(t)
	cl, err := Connect(Account{
		ImapHost: host, ImapPort: port, ImapTLS: "",
		Username: "u", Password: "p",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cl.Close()
	if _, err := cl.Select("INBOX"); err != nil {
		t.Fatalf("select: %v", err)
	}
	type res struct {
		msgs []Msg
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		msgs, err := cl.FetchEnvelopes([]uint32{1, 2})
		ch <- res{msgs, err}
	}()
	var got []Msg
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("fetch envelopes: %v", r.err)
		}
		got = r.msgs
	case <-time.After(15 * time.Second):
		t.Fatal("fetch envelopes timed out")
	}
	byUID := map[uint32]Msg{}
	for _, m := range got {
		byUID[m.UID] = m
	}
	if byUID[1].MessageID != fakeDoubleBracketID {
		t.Errorf("double-bracket header form: MessageID = %q, want %q", byUID[1].MessageID, fakeDoubleBracketID)
	}
	if byUID[2].MessageID != "normal-1@example.com" {
		t.Errorf("normal header form: MessageID = %q, want %q", byUID[2].MessageID, "normal-1@example.com")
	}
}
