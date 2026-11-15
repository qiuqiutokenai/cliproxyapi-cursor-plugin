package main

import "testing"

func TestConnectEndMetadataIsSuccessful(t *testing.T) {
	for _, body := range []string{``, `{}`, `{"metadata":{"x-request-id":["fixture"]}}`, `{"error":null,"metadata":{"x-fixture":["ok"]}}`} {
		if err := connectError(200, []byte(body)); err != nil {
			t.Errorf("valid end metadata %s: %v", body, err)
		}
	}
}

func TestConnectErrorPreservesExplicitClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		{"weekly quota", 429, `{"code":"resource_exhausted","message":"Model not included in weekly usage quota"}`, 429, "resource_exhausted"},
		{"model permission", 403, `{"code":"permission_denied","message":"Model blocked: ask your admin to enable access"}`, 403, "permission_denied"},
		{"unknown quota code", 429, `{"code":"usage_model_mismatch","message":"not included in weekly usage"}`, 429, "usage_model_mismatch"},
		{"model blocked http", 403, `{"code":"model_blocked","message":"Model Blocked"}`, 403, "model_blocked"},
		{"model transport quota", 429, `{"message":"model not found for weekly quota"}`, 429, "cursor_error"},
		{"server response with model text", 503, `{"code":"backend_overloaded","message":"Model blocked: quota expired"}`, 503, "backend_overloaded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := connectError(tc.status, []byte(tc.body))
			detail := errorDetail(err)
			if detail.HTTPStatus != tc.wantStatus || detail.Code != tc.wantCode || detail.CredentialScoped {
				t.Fatalf("detail = %+v, want status=%d code=%s and model-scoped", detail, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestConnectQuotaScopeRequiresExplicitEvidence(t *testing.T) {
	for _, tc := range []struct {
		body       string
		credential bool
	}{
		{`{"code":"resource_exhausted","message":"weekly quota exceeded"}`, false},
		{`{"code":"resource_exhausted","message":"account-wide quota exceeded"}`, false},
		{`{"code":"resource_exhausted","message":"quota exceeded","credential_scoped":true}`, true},
	} {
		detail := errorDetail(connectError(429, []byte(tc.body)))
		if detail.CredentialScoped != tc.credential {
			t.Fatalf("credential scope = %v, want %v", detail.CredentialScoped, tc.credential)
		}
	}
}
