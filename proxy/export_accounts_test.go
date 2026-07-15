package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApiExportAccountsPreservesEnterprisePowerProfile(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	profileArn := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/example"
	account := config.Account{
		ID:                "enterprise-power",
		AuthMethod:        "idc",
		Provider:          "Enterprise",
		Region:            "us-east-1",
		StartUrl:          "https://d-example.awsapps.com/start",
		ProfileRegionHint: "eu-central-1",
		ProfileArn:        profileArn,
		SubscriptionType:  "POWER",
		SubscriptionTitle: "KIRO POWER",
		Enabled:           true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}

	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/accounts/export", strings.NewReader(`{"ids":["enterprise-power"]}`))
	rec := httptest.NewRecorder()
	h.apiExportAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var data struct {
		Accounts []struct {
			Idp               string `json:"idp"`
			ProfileArn        string `json:"profileArn"`
			ProfileRegionHint string `json:"profileRegionHint"`
			StartURL          string `json:"startUrl"`
			Credentials       struct {
				Provider          string `json:"provider"`
				ProfileArn        string `json:"profileArn"`
				ProfileRegionHint string `json:"profileRegionHint"`
				StartURL          string `json:"startUrl"`
			} `json:"credentials"`
			Subscription struct {
				Type string `json:"type"`
			} `json:"subscription"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(data.Accounts) != 1 {
		t.Fatalf("expected one account, got %d", len(data.Accounts))
	}
	got := data.Accounts[0]
	if got.Idp != "Enterprise" || got.Credentials.Provider != "Enterprise" {
		t.Fatalf("unexpected identity export: %+v", got)
	}
	if got.ProfileArn != profileArn || got.Credentials.ProfileArn != profileArn {
		t.Fatalf("profile ARN missing from export: %+v", got)
	}
	if got.ProfileRegionHint != "eu-central-1" || got.Credentials.ProfileRegionHint != "eu-central-1" {
		t.Fatalf("profile region missing from export: %+v", got)
	}
	if got.StartURL != account.StartUrl || got.Credentials.StartURL != account.StartUrl {
		t.Fatalf("start URL missing from export: %+v", got)
	}
	if got.Subscription.Type != "POWER" {
		t.Fatalf("subscription type = %q, want POWER", got.Subscription.Type)
	}
}

func TestApiExportAccountsDefaultsMissingIDCProviderToEnterprise(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "legacy-idc", AuthMethod: "idc", Region: "us-east-1", Enabled: true}); err != nil {
		t.Fatalf("config.AddAccount: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/accounts/export", strings.NewReader(`{"ids":["legacy-idc"]}`))
	rec := httptest.NewRecorder()
	(&Handler{}).apiExportAccounts(rec, req)
	if !strings.Contains(rec.Body.String(), `"idp":"Enterprise"`) {
		t.Fatalf("legacy IdC account was not exported as Enterprise: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider":"Enterprise"`) {
		t.Fatalf("legacy IdC credentials were not exported with Enterprise provider: %s", rec.Body.String())
	}
}
