package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
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
	message := codeMessage(s.From, to, code)
	if _, err := strings.NewReader(message).WriteTo(w); err != nil {
		return err
	}
	return w.Close()
}

func codeMessage(from, to, code string) string {
	const boundary = "shortlog-sign-in-code"
	plain := fmt.Sprintf("Your Shortlog sign-in code: %s\r\n\r\nExpires in 10 minutes. If you didn't request this, ignore this email.\r\n", code)
	htmlBody := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="margin:0;padding:32px 16px;background:#f6f6f4;color:#1d1d1b;font-family:Arial,Helvetica,sans-serif;">
  <div style="max-width:440px;margin:0 auto;padding:32px;background:#ffffff;border:1px solid #e9e9e5;border-radius:12px;">
    <p style="margin:0 0 28px;font-size:14px;font-weight:700;letter-spacing:.04em;">SHORTLOG</p>
    <h1 style="margin:0 0 12px;font-size:24px;line-height:1.25;">Your sign-in code</h1>
    <p style="margin:0 0 24px;color:#555550;font-size:15px;line-height:1.5;">Enter this code to sign in. It expires in 10 minutes.</p>
    <p style="margin:0 0 24px;padding:16px;background:#f6f6f4;border-radius:8px;text-align:center;font-size:30px;font-weight:700;letter-spacing:.15em;line-height:1.3;">%s</p>
    <p style="margin:0;color:#777771;font-size:13px;line-height:1.5;">Didn't request this? You can safely ignore this email.</p>
  </div>
</body>
</html>`, html.EscapeString(code))
	htmlBody = strings.ReplaceAll(htmlBody, "\n", "\r\n")
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Your Shortlog sign-in code\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=%q\r\n\r\n--%s\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n--%s\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n--%s--\r\n", from, to, boundary, boundary, plain, boundary, htmlBody, boundary)
}
