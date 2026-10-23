package mail

import (
	"bytes"
	"fmt"
	"net"
	"strconv"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func SendSMTP(acct Account, from string, rcpts []string, msg []byte) error {
	addr := net.JoinHostPort(acct.SMTPHost, strconv.Itoa(acct.SMTPPort))
	tlsConf := TLSConfigFor(acct.SMTPHost, acct.TLSSkipVerify)

	var (
		c   *smtp.Client
		err error
	)
	switch acct.SMTPTLS {
	case "tls":
		c, err = smtp.DialTLS(addr, tlsConf)
	case "starttls":
		c, err = smtp.DialStartTLS(addr, tlsConf)
	default:
		c, err = smtp.Dial(addr)
	}
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("AUTH"); ok {
		auth := sasl.NewPlainClient("", acct.Username, acct.Password)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.SendMail(from, rcpts, bytes.NewReader(msg)); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return c.Quit()
}
