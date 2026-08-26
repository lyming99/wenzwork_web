package mailer

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteMessageEncodesSubjectAndRejectsHeaderInjection(t *testing.T) {
	sender, err := NewSMTPSender(SMTPConfig{
		Host: "localhost",
		Port: 1025,
		From: "WenzWork <noreply@example.test>",
	})
	if err != nil {
		t.Fatalf("NewSMTPSender() error = %v", err)
	}
	to, err := parseMailbox("User <user@example.test>")
	if err != nil {
		t.Fatalf("parseMailbox() error = %v", err)
	}
	var output bytes.Buffer
	if err := writeMessage(&output, sender.from, to, Message{Subject: "验证你的邮箱", Text: "第一行\n第二行"}); err != nil {
		t.Fatalf("writeMessage() error = %v", err)
	}
	for _, value := range []string{"Subject: =?UTF-8?", "Content-Type: text/plain", "第一行\r\n第二行"} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("mail output missing %q:\n%s", value, output.String())
		}
	}

	if _, err := parseMailbox("victim@example.test\r\nBcc: attacker@example.test"); err == nil {
		t.Fatal("parseMailbox() accepted header injection")
	}
}
