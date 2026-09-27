package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTP sends through implicit TLS (SMTPS), as used by Hostinger on port 465.
type SMTP struct {
	Address, Username, Password, From string
}

func (s SMTP) SendCode(ctx context.Context, to, code string) error {
	host, _, err := net.SplitHostPort(s.Address)
	if err != nil {
		return err
	}
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}}).DialContext(ctx, "tcp", s.Address)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
		return err
	}
	if err := client.Mail(s.From); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	message := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Your Shortlog sign-in code\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nYour Shortlog sign-in code is %s.\r\nIt expires in 10 minutes. If you did not request this, ignore this email.\r\n", s.From, to, code)
	if _, err := strings.NewReader(message).WriteTo(w); err != nil {
		return err
	}
	return w.Close()
}
