package providers

import (
	"errors"
	"strings"
	"testing"
)

func multiField() Provider {
	return Provider{
		Key: "stub",
		SecretFields: []SecretField{
			{Name: "client_id", Label: "Client ID", Env: "STUB_CLIENT_ID"},
			{Name: "client_secret", Label: "Client secret", Env: "STUB_CLIENT_SECRET"},
		},
	}
}

func TestValidateAcceptsAllDeclaredFields(t *testing.T) {
	err := multiField().Validate(map[string]string{
		"client_id":     "id-1",
		"client_secret": "sec-1",
	})
	if err != nil {
		t.Fatalf("valid fields rejected: %v", err)
	}
}

func TestValidateRejectsUnknownField(t *testing.T) {
	err := multiField().Validate(map[string]string{
		"client_id":     "id-1",
		"client_secret": "sec-1",
		"surprise":      "x",
	})
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("want ErrUnknownField, got %v", err)
	}
}

func TestValidateRejectsMissingOrEmptyField(t *testing.T) {
	// Absent field.
	if err := multiField().Validate(map[string]string{"client_id": "id-1"}); !errors.Is(err, ErrMissingField) {
		t.Fatalf("absent field: want ErrMissingField, got %v", err)
	}
	// Present but whitespace-only.
	err := multiField().Validate(map[string]string{"client_id": "id-1", "client_secret": "   "})
	if !errors.Is(err, ErrMissingField) {
		t.Fatalf("blank field: want ErrMissingField, got %v", err)
	}
}

func TestValidateRejectsOversizeValue(t *testing.T) {
	big := strings.Repeat("a", MaxFieldValueLen+1)
	err := Provider{SecretFields: []SecretField{{Name: "api_key"}}}.Validate(map[string]string{"api_key": big})
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("want ErrFieldTooLong, got %v", err)
	}
}
