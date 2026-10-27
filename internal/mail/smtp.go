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

var smtpTimeout = 90 * time.Second

const smtpDialTimeout = 30 * time.Second

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
