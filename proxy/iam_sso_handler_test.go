package proxy

import (
	"kiro-go/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeIamSsoStartRequestUsesLegacyRegionAsAuthRegion(t *testing.T) {
	req := iamSsoStartRequest{
		Region:        "us-east-1",
		ProfileRegion: "eu-central-1",
	}
	req.normalize()
	if req.AuthRegion != "us-east-1" || req.ProfileRegion != "eu-central-1" {
		t.Fatalf("unexpected normalized request: %+v", req)
	}
}

func TestApiStartIamSsoRequiresProfileRegion(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/auth/iam-sso/start", strings.NewReader(
		`{"startUrl":"https://d-example.awsapps.com/start","authRegion":"us-east-1"}`,
	))
	rec := httptest.NewRecorder()
	h.apiStartIamSso(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing profile region, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBuildIamSsoAccountPersistsEnterpriseRoutingFields(t *testing.T) {
	account := buildIamSsoAccount(auth.IamSsoResult{
		AccessToken:       "access",
		RefreshToken:      "refresh",
		ClientID:          "client",
		ClientSecret:      "secret",
		AuthRegion:        "us-east-1",
		ProfileRegionHint: "eu-central-1",
		StartURL:          "https://d-example.awsapps.com/start",
		ExpiresIn:         3600,
	})
	if account.AuthMethod != "idc" || account.Provider != "Enterprise" {
		t.Fatalf("unexpected identity fields: %+v", account)
	}
	if account.Region != "us-east-1" || account.ProfileRegionHint != "eu-central-1" {
		t.Fatalf("unexpected routing fields: %+v", account)
	}
	if account.StartUrl != "https://d-example.awsapps.com/start" {
		t.Fatalf("start URL not persisted: %+v", account)
	}
}
