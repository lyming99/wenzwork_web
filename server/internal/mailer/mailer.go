package mailer

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string
}

type Sender interface {
	Send(context.Context, Message) error
}

type SMTPConfig struct {
	Host       string
	Port       int
	Username   string
	Password   string
	From       string
	RequireTLS bool
	Timeout    time.Duration
}

type SMTPSender struct {
	config SMTPConfig
	from   *mail.Address
}

func NewSMTPSender(config SMTPConfig) (*SMTPSender, error) {
	config.Host = strings.TrimSpace(config.Host)
	if config.Host == "" || config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("valid SMTP host and port are required")
	}
	from, err := mail.ParseAddress(config.From)
	if err != nil {
		return nil, fmt.Errorf("parse MAIL_FROM: %w", err)
	}
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Second
	}
	return &SMTPSender{config: config, from: from}, nil
}

func (s *SMTPSender) Send(ctx context.Context, message Message) error {
	to, err := parseMailbox(message.To)
	if err != nil {
		return err
	}
	if strings.ContainsAny(message.Subject, "\r\n") {
		return errors.New("mail subject contains a header break")
	}

	address := net.JoinHostPort(s.config.Host, strconv.Itoa(s.config.Port))
	dialer := net.Dialer{Timeout: s.config.Timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("connect SMTP server: %w", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(s.config.Timeout)); err != nil {
		return fmt.Errorf("set SMTP deadline: %w", err)
	}

	client, err := smtp.NewClient(connection, s.config.Host)
	if err != nil {
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		tlsConfig := &tls.Config{ServerName: s.config.Host, MinVersion: tls.VersionTLS12}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("start SMTP TLS: %w", err)
		}
	} else if s.config.RequireTLS {
		return errors.New("SMTP server does not advertise STARTTLS")
	}

	if s.config.Username != "" {
		auth := smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("authenticate SMTP client: %w", err)
		}
	}
	if err := client.Mail(s.from.Address); err != nil {
		return fmt.Errorf("set SMTP sender: %w", err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return fmt.Errorf("set SMTP recipient: %w", err)
	}

	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("open SMTP message: %w", err)
	}
	if err := writeMessage(wc, s.from, to, message); err != nil {
		_ = wc.Close()
		return err
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("finish SMTP message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("quit SMTP client: %w", err)
	}
	return nil
}

func parseMailbox(raw string) (*mail.Address, error) {
	address, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil || address.Address == "" {
		return nil, errors.New("valid recipient email is required")
	}
	return address, nil
}

func writeMessage(destination io.Writer, from, to *mail.Address, message Message) error {
	writer := bufio.NewWriter(destination)
	headers := []string{
		"From: " + from.String(),
		"To: " + to.String(),
		"Subject: " + mime.QEncoding.Encode("UTF-8", message.Subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
	}
	for _, header := range headers {
		if _, err := writer.WriteString(header + "\r\n"); err != nil {
			return fmt.Errorf("write mail header: %w", err)
		}
	}
	if _, err := writer.WriteString("\r\n" + strings.ReplaceAll(message.Text, "\n", "\r\n")); err != nil {
		return fmt.Errorf("write mail body: %w", err)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush mail message: %w", err)
	}
	return nil
}
