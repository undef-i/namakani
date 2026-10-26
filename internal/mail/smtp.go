package mail

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// smtpTimeout is the connection-level budget for one SMTP session: no
// single command, submission wait, or I/O stall may exceed it. It is
// the same budget used for the provider HTTP clients (90s, net/http's
// DefaultTransport IdleConnTimeout), instead of go-smtp's RFC 5321
// defaults of 5 minutes per command and 12 minutes per submission.
// It is a var so tests can shrink it.
var smtpTimeout = 90 * time.Second

// smtpDialTimeout bounds connection establishment. It matches the
// dialer go-smtp itself uses by default (30s).
const smtpDialTimeout = 30 * time.Second

// deadlineConn re-arms the connection deadline before every Read and
// Write. go-smtp sets and clears deadlines around its own command
// phases; without this, a peer that stalls mid-DATA (outside any
// command phase) would block the send path indefinitely.
type deadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(b)
}

func dialSMTP(acct Account) (*smtp.Client, error) {
	addr := net.JoinHostPort(acct.SMTPHost, strconv.Itoa(acct.SMTPPort))
	tlsConf := TLSConfigFor(acct.SMTPHost, acct.TLSSkipVerify)
	dialer := &net.Dialer{Timeout: smtpDialTimeout}

	var (
		c   *smtp.Client
		err error
	)
	switch acct.SMTPTLS {
	case "tls":
		var conn net.Conn
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConf}).Dial("tcp", addr)
		if err == nil {
			c = smtp.NewClient(&deadlineConn{conn, smtpTimeout})
		}
	case "starttls":
		var conn net.Conn
		conn, err = dialer.Dial("tcp", addr)
		if err == nil {
			c, err = smtp.NewClientStartTLS(&deadlineConn{conn, smtpTimeout}, tlsConf)
		}
	default:
		var conn net.Conn
		conn, err = dialer.Dial("tcp", addr)
		if err == nil {
			c = smtp.NewClient(&deadlineConn{conn, smtpTimeout})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("smtp connect: %w", err)
	}
	c.CommandTimeout = smtpTimeout
	c.SubmissionTimeout = smtpTimeout

	if ok, _ := c.Extension("AUTH"); ok {
		auth := sasl.NewPlainClient("", acct.Username, acct.Password)
		if err := c.Auth(auth); err != nil {
			c.Close()
			return nil, fmt.Errorf("smtp auth: %w", err)
		}
	}
	return c, nil
}

func SendSMTP(acct Account, from string, rcpts []string, msg []byte) error {
	c, err := dialSMTP(acct)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.SendMail(from, rcpts, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return c.Quit()
}

func TestSMTP(acct Account) error {
	c, err := dialSMTP(acct)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Quit()
}
