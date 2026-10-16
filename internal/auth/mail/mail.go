// Package mail sends Auth's emails: the sign-in codes. In development the emails go
// to the log; in the cloud, to an SMTP provider.
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// Message is a plain-text email.
type Message struct {
	To      string
	Subject string
	Text    string
}

// Mailer sends emails.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Log writes emails to the log instead of sending them.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("email (not sent: the log mailer is on)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return nil
}

// SMTP sends emails through an SMTP server with STARTTLS: the submission port 587.
// Implicit TLS on port 465 is not supported.
type SMTP struct {
	Addr     string // host:port
	Username string
	Password string
	From     string
}

func (s SMTP) Send(ctx context.Context, m Message) error {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return err
	}
	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, host)
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("sender %q: %w", s.From, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n",
		from.String(), m.To, m.Subject, time.Now().Format(time.RFC1123Z))
	b.WriteString(strings.ReplaceAll(m.Text, "\n", "\r\n"))
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(s.Addr, auth, from.Address, []string{m.To}, []byte(b.String())) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Parse builds a mailer from its spec: "log", or smtp://user:password@host:587.
func Parse(spec, from string, log *slog.Logger) (Mailer, error) {
	if spec == "" || spec == "log" {
		return Log{Logger: log}, nil
	}
	u, err := url.Parse(spec)
	if err != nil || u.Scheme != "smtp" || u.Host == "" {
		return nil, fmt.Errorf("mail %q: want \"log\" or smtp://user:password@host:port", spec)
	}
	if from == "" {
		return nil, fmt.Errorf("mail: the sender address is not set")
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "587")
	}
	s := SMTP{Addr: addr, From: from}
	if u.User != nil {
		s.Username = u.User.Username()
		s.Password, _ = u.User.Password()
	}
	return s, nil
}
