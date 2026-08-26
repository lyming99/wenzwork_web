package membership

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
	"gorm.io/gorm"
)

type recordingPromotionSender struct {
	messages []mailer.Message
	err      error
}

func (s *recordingPromotionSender) Send(_ context.Context, message mailer.Message) error {
	s.messages = append(s.messages, message)
	return s.err
}

func TestNormalizePromotionEmail(t *testing.T) {
	normalized, err := normalizePromotionEmail("  Member@Example.COM ")
	if err != nil {
		t.Fatalf("normalizePromotionEmail() error = %v", err)
	}
	if normalized != "member@example.com" {
		t.Fatalf("normalized email = %q", normalized)
	}

	for _, invalid := range []string{"", "not-an-email", "Name <member@example.com>", "a@@example.com"} {
		if _, err := normalizePromotionEmail(invalid); !errors.Is(err, ErrBetaPromotionInvalidEmail) {
			t.Fatalf("normalizePromotionEmail(%q) error = %v, want invalid email", invalid, err)
		}
	}
}

func TestBetaPromotionCodeCipherRoundTripAndAuthentication(t *testing.T) {
	codec, err := NewCodeCodec([]byte(strings.Repeat("promotion-code-hmac-key-", 2)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	service, err := NewBetaPromotionService(
		&gorm.DB{}, codec, &recordingPromotionSender{}, strings.Repeat("promotion-encryption-key-", 2),
	)
	if err != nil {
		t.Fatalf("NewBetaPromotionService() error = %v", err)
	}

	claimID := uuid.New()
	plaintext := []byte("WZM-2345-6789-ABCD-EFGH-JKMN")
	ciphertext, err := service.encryptCode(claimID, plaintext)
	if err != nil {
		t.Fatalf("encryptCode() error = %v", err)
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext contains redemption code plaintext")
	}
	decrypted, err := service.decryptCode(claimID, ciphertext)
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decryptCode() = %q, %v", decrypted, err)
	}

	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 1
	if _, err := service.decryptCode(claimID, tampered); err == nil {
		t.Fatal("decryptCode() accepted tampered ciphertext")
	}
	if _, err := service.decryptCode(uuid.New(), ciphertext); err == nil {
		t.Fatal("decryptCode() accepted ciphertext for another claim")
	}
}

func TestBetaPromotionMessageContainsCodeAndGroupContacts(t *testing.T) {
	message := betaPromotionMessage("member@example.com", "WZM-2345-6789-ABCD-EFGH-JKMN")
	for _, expected := range []string{
		"1 年 Pro", "WZM-2345-6789-ABCD-EFGH-JKMN", "member@example.com",
		"只能由使用该邮箱注册且已验证的 WenzWork 账号兑换", "lyming555", "44185539",
	} {
		if !strings.Contains(message.Subject+message.Text, expected) {
			t.Fatalf("promotion email does not contain %q", expected)
		}
	}
}

func TestRedemptionEmailsMatch(t *testing.T) {
	if !redemptionEmailsMatch(" Member@Example.COM ", "member@example.com") {
		t.Fatal("redemptionEmailsMatch() rejected the same normalized email")
	}
	if redemptionEmailsMatch("other@example.com", "member@example.com") {
		t.Fatal("redemptionEmailsMatch() accepted a different account email")
	}
}

func TestValidateBetaRedemptionEligibility(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	expiresAt := now.AddDate(1, 0, 0)

	tests := []struct {
		name                string
		isBetaCode          bool
		previousRedemptions int64
		current             *Membership
		wantErr             error
	}{
		{name: "fresh beta account", isBetaCode: true},
		{name: "beta already used by email", isBetaCode: true, previousRedemptions: 1, wantErr: ErrCodeUnavailable},
		{name: "beta blocked for lifetime member", isBetaCode: true, current: &Membership{PlanCode: "pro", PlanRank: 10, StartsAt: now, ExpiresAt: nil}, wantErr: ErrMembershipNotExtended},
		{name: "beta allowed for expiring member", isBetaCode: true, current: &Membership{PlanCode: "pro", PlanRank: 10, StartsAt: now, ExpiresAt: &expiresAt}},
		{name: "ordinary code keeps existing lifetime behavior", current: &Membership{PlanCode: "pro", PlanRank: 10, StartsAt: now, ExpiresAt: nil}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateBetaRedemptionEligibility(test.isBetaCode, test.previousRedemptions, test.current)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("validateBetaRedemptionEligibility() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestBetaPromotionAdminRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	service := &BetaPromotionService{}
	for _, filter := range []BetaPromotionClaimFilter{
		{Limit: 0},
		{Limit: 101},
		{Limit: 50, Offset: -1},
		{Limit: 50, DeliveryStatus: "unknown"},
		{Limit: 50, RedemptionStatus: "unknown"},
		{Limit: 50, Query: strings.Repeat("a", 321)},
	} {
		if _, err := service.ListAdminClaims(context.Background(), filter); !errors.Is(err, ErrBetaPromotionAdminInvalid) {
			t.Fatalf("ListAdminClaims(%+v) error = %v, want invalid input", filter, err)
		}
	}
	if _, err := service.UpdateAdminRemaining(context.Background(), uuid.Nil, 0); !errors.Is(err, ErrBetaPromotionAdminInvalid) {
		t.Fatalf("UpdateAdminRemaining(nil actor) error = %v, want invalid input", err)
	}
	if _, err := service.UpdateAdminRemaining(context.Background(), uuid.New(), -1); !errors.Is(err, ErrBetaPromotionAdminInvalid) {
		t.Fatalf("UpdateAdminRemaining(-1) error = %v, want invalid input", err)
	}
	if _, err := service.UpdateAdminRemaining(context.Background(), uuid.New(), betaPromotionMaxQuota+1); !errors.Is(err, ErrBetaPromotionAdminInvalid) {
		t.Fatalf("UpdateAdminRemaining(over max) error = %v, want invalid input", err)
	}
	if _, err := service.UpdateAdminGroupQRCode(context.Background(), uuid.Nil, "image/png", nil); !errors.Is(err, ErrBetaPromotionAdminInvalid) {
		t.Fatalf("UpdateAdminGroupQRCode(nil actor) error = %v, want invalid input", err)
	}
	if _, err := service.UpdateAdminGroupQRCode(context.Background(), uuid.New(), "image/png", []byte("not an image")); !errors.Is(err, ErrBetaPromotionGroupQRCodeInvalid) {
		t.Fatalf("UpdateAdminGroupQRCode(invalid image) error = %v, want invalid QR code", err)
	}
	if _, err := service.RemoveAdminGroupQRCode(context.Background(), uuid.Nil); !errors.Is(err, ErrBetaPromotionAdminInvalid) {
		t.Fatalf("RemoveAdminGroupQRCode(nil actor) error = %v, want invalid input", err)
	}
}

func TestValidateBetaPromotionGroupQRCode(t *testing.T) {
	validPNG := encodePromotionTestPNG(t, 128, 128)
	contentType, err := validateBetaPromotionGroupQRCode(" IMAGE/PNG ", validPNG)
	if err != nil || contentType != "image/png" {
		t.Fatalf("validateBetaPromotionGroupQRCode(valid PNG) = %q, %v", contentType, err)
	}

	for _, test := range []struct {
		name        string
		contentType string
		content     []byte
	}{
		{name: "empty", contentType: "image/png"},
		{name: "invalid bytes", contentType: "image/png", content: []byte("not an image")},
		{name: "type mismatch", contentType: "image/jpeg", content: validPNG},
		{name: "unsupported type", contentType: "image/gif", content: validPNG},
		{
			name: "oversized bytes", contentType: "image/png",
			content: make([]byte, BetaPromotionGroupQRCodeMaxBytes+1),
		},
		{
			name: "oversized dimensions", contentType: "image/png",
			content: encodePromotionTestPNG(t, betaPromotionMaxQRDimension+1, 1),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateBetaPromotionGroupQRCode(test.contentType, test.content); !errors.Is(err, ErrBetaPromotionGroupQRCodeInvalid) {
				t.Fatalf("validateBetaPromotionGroupQRCode() error = %v, want invalid QR code", err)
			}
		})
	}
}

func TestBetaPromotionGroupQRCodeURLRequiresCompleteConfiguration(t *testing.T) {
	now := time.Date(2026, 7, 24, 8, 30, 0, 0, time.UTC)
	contentType := "image/png"
	campaign := betaPromotionCampaignRow{
		GroupQRCode: []byte("image"), GroupQRCodeContentType: &contentType,
		GroupQRCodeUpdatedAt: &now,
	}
	url := betaPromotionGroupQRCodeURL(campaign)
	if url == nil || !strings.Contains(*url, "/api/v1/promotions/beta-pro/group-qr?v=") {
		t.Fatalf("betaPromotionGroupQRCodeURL() = %v", url)
	}
	campaign.GroupQRCode = nil
	if betaPromotionGroupQRCodeURL(campaign) != nil {
		t.Fatal("betaPromotionGroupQRCodeURL() returned URL for incomplete configuration")
	}
}

func encodePromotionTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, width, height))
	value.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}
	return encoded.Bytes()
}
