package membership

import (
	"errors"
	"net/mail"
	"strings"
)

var errRecipientEmailInvalid = errors.New("recipient email is invalid")

func normalizeRecipientEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 320 || strings.Count(trimmed, "@") != 1 {
		return "", errRecipientEmailInvalid
	}
	parsed, err := mail.ParseAddress(trimmed)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, trimmed) {
		return "", errRecipientEmailInvalid
	}
	parts := strings.Split(parsed.Address, "@")
	if parts[0] == "" || parts[1] == "" || strings.ContainsAny(parsed.Address, "\r\n\t ") {
		return "", errRecipientEmailInvalid
	}
	return strings.ToLower(parsed.Address), nil
}
