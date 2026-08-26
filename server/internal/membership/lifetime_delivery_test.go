package membership

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestLifetimeCodeDeliveryMessageContainsPermanentCodeAndSupport(t *testing.T) {
	message := lifetimeCodeDeliveryMessage(
		"buyer@example.com",
		"WZM-2345-6789-ABCD-EFGH-JKMN",
	)
	for _, expected := range []string{
		"永久 Pro", "WZM-2345-6789-ABCD-EFGH-JKMN",
		"账户中心 → 会员中心", "lyming555", "44185539",
	} {
		if !strings.Contains(message.Subject+message.Text, expected) {
			t.Fatalf("lifetime code email does not contain %q", expected)
		}
	}
}

func TestLifetimeDeliveryBatchNameStaysWithinDatabaseLimit(t *testing.T) {
	name := lifetimeDeliveryBatchName(
		strings.Repeat("long-recipient-", 20)+"@example.com",
		"ABCD",
	)
	if len([]rune(name)) > 120 {
		t.Fatalf("batch name has %d runes, want at most 120", len([]rune(name)))
	}
	if !strings.Contains(name, lifetimeCodeDeliveryBatchPrefix) || !strings.HasSuffix(name, "ABCD") {
		t.Fatalf("unexpected batch name %q", name)
	}
}

func TestLifetimeCodeDeliveryRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	service := &LifetimeCodeDeliveryService{}
	if _, err := service.ListLifetimeCodeDeliveries(context.Background(), 0); !errors.Is(err, ErrLifetimeCodeDeliveryInvalid) {
		t.Fatalf("ListLifetimeCodeDeliveries() error = %v, want invalid input", err)
	}
	if _, err := service.SendLifetimeCode(context.Background(), LifetimeCodeDeliveryInput{
		RequestID: uuid.New(), Email: "not-an-email", ActorUserID: uuid.New(),
	}); !errors.Is(err, ErrLifetimeCodeDeliveryInvalid) {
		t.Fatalf("SendLifetimeCode() error = %v, want invalid input", err)
	}
}
