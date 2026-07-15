package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStartIamSsoLoginSeparatesAuthAndProfileRegions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/register" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clientId":"client-1","clientSecret":"secret-1"}`))
	}))
	defer srv.Close()

	previous := iamOIDCBaseURL
	iamOIDCBaseURL = func(region string) string {
		if region != "us-east-1" {
			t.Fatalf("OIDC region = %q, want us-east-1", region)
		}
		return srv.URL
	}
	defer func() { iamOIDCBaseURL = previous }()

	sessionID, authorizeURL, _, err := StartIamSsoLogin(
		"https://d-example.awsapps.com/start",
		"us-east-1",
		"eu-central-1",
	)
	if err != nil {
		t.Fatalf("StartIamSsoLogin: %v", err)
	}
	defer CancelIamSsoLogin(sessionID)

	sessionsMu.RLock()
	session := sessions[sessionID]
	sessionsMu.RUnlock()
	if session == nil {
		t.Fatal("IAM SSO session not found")
	}
	if session.AuthRegion != "us-east-1" || session.ProfileRegionHint != "eu-central-1" {
		t.Fatalf("unexpected session regions: %+v", session)
	}
	if !strings.Contains(authorizeURL, "client_id=client-1") {
		t.Fatalf("authorize URL missing client id: %s", authorizeURL)
	}
}

func TestValidateIamSsoLoginInputAcceptsSupportedStartURLs(t *testing.T) {
	startURLs := []string{
		"https://d-example.awsapps.com/start",
		"https://company-name.awsapps.com/start/",
		"https://identitycenter.amazonaws.com/ssoins-example1234567890",
	}

	for _, startURL := range startURLs {
		t.Run(startURL, func(t *testing.T) {
			if err := ValidateIamSsoLoginInput(startURL, "us-east-1", "eu-central-1"); err != nil {
				t.Fatalf("ValidateIamSsoLoginInput(%q): %v", startURL, err)
			}
		})
	}
}

func TestValidateIamSsoLoginInputRejectsUnsupportedStartURLs(t *testing.T) {
	startURLs := []string{
		"http://d-example.awsapps.com/start",
		"https://d-example.awsapps.com.evil.example/start",
		"https://user@d-example.awsapps.com/start",
		"https://d-example.awsapps.com:443/start",
		"https://d-example.awsapps.com/start?region=eu-central-1",
		"https://d-example.awsapps.com/start#fragment",
		"https://d-example.awsapps.com/not-start",
		"https://ssoins-example1234567890.portal.eu-central-1.app.aws",
		"https://identitycenter.amazonaws.com/not-an-issuer",
	}

	for _, startURL := range startURLs {
		t.Run(startURL, func(t *testing.T) {
			if err := ValidateIamSsoLoginInput(startURL, "us-east-1", "eu-central-1"); err == nil {
				t.Fatalf("expected unsupported start URL %q to be rejected", startURL)
			}
		})
	}
}

func TestValidateIamSsoLoginInputRejectsInvalidRegions(t *testing.T) {
	tests := []struct {
		name          string
		authRegion    string
		profileRegion string
	}{
		{name: "auth region is URL", authRegion: "https://oidc.us-east-1.amazonaws.com", profileRegion: "eu-central-1"},
		{name: "auth region contains path", authRegion: "us-east-1/token", profileRegion: "eu-central-1"},
		{name: "profile region contains port", authRegion: "us-east-1", profileRegion: "eu-central-1:443"},
		{name: "profile region is empty", authRegion: "us-east-1", profileRegion: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateIamSsoLoginInput("https://d-example.awsapps.com/start", tt.authRegion, tt.profileRegion); err == nil {
				t.Fatalf("expected regions auth=%q profile=%q to be rejected", tt.authRegion, tt.profileRegion)
			}
		})
	}
}

func TestCompleteIamSsoLoginUsesAuthRegionForTokenExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/client/register":
			_, _ = w.Write([]byte(`{"clientId":"client-2","clientSecret":"secret-2"}`))
		case "/token":
			_, _ = w.Write([]byte(`{"accessToken":"access-2","refreshToken":"refresh-2","expiresIn":3600}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	previous := iamOIDCBaseURL
	iamOIDCBaseURL = func(region string) string {
		if region != "us-east-1" {
			t.Fatalf("OIDC region = %q, want us-east-1", region)
		}
		return srv.URL
	}
	defer func() { iamOIDCBaseURL = previous }()

	sessionID, _, _, err := StartIamSsoLogin(
		"https://d-example.awsapps.com/start",
		"us-east-1",
		"eu-central-1",
	)
	if err != nil {
		t.Fatalf("StartIamSsoLogin: %v", err)
	}

	sessionsMu.RLock()
	state := sessions[sessionID].State
	sessionsMu.RUnlock()
	callbackURL := fmt.Sprintf("http://127.0.0.1/oauth/callback?code=code-2&state=%s", state)
	result, err := CompleteIamSsoLogin(sessionID, callbackURL)
	if err != nil {
		t.Fatalf("CompleteIamSsoLogin: %v", err)
	}
	if result.AccessToken != "access-2" || result.RefreshToken != "refresh-2" || result.AuthRegion != "us-east-1" {
		t.Fatalf("unexpected completion result: %+v", result)
	}
}

func TestCompleteIamSsoLoginClearsExpiredSessionSecrets(t *testing.T) {
	sessionID := "expired-session"
	session := &IamSsoSession{
		ClientID:     "client-secret-id",
		ClientSecret: "client-secret-value",
		CodeVerifier: "verifier",
		State:        "state",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}

	sessionsMu.Lock()
	sessions[sessionID] = session
	sessionsMu.Unlock()

	_, err := CompleteIamSsoLogin(sessionID, "http://127.0.0.1/oauth/callback")
	if err == nil {
		t.Fatal("expected expired session error")
	}
	if session.ClientID != "" || session.ClientSecret != "" || session.CodeVerifier != "" || session.State != "" {
		t.Fatalf("expired session retained sensitive fields: %+v", session)
	}
}

func TestCompleteIamSsoLoginReturnsRoutingMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/client/register":
			_, _ = w.Write([]byte(`{"clientId":"client-3","clientSecret":"secret-3"}`))
		case "/token":
			_, _ = w.Write([]byte(`{"accessToken":"access-3","refreshToken":"refresh-3","expiresIn":3600}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	previous := iamOIDCBaseURL
	iamOIDCBaseURL = func(string) string { return srv.URL }
	defer func() { iamOIDCBaseURL = previous }()

	startURL := "https://d-example.awsapps.com/start"
	sessionID, _, _, err := StartIamSsoLogin(startURL, "us-east-1", "eu-central-1")
	if err != nil {
		t.Fatalf("StartIamSsoLogin: %v", err)
	}
	sessionsMu.RLock()
	state := sessions[sessionID].State
	sessionsMu.RUnlock()

	result, err := CompleteIamSsoLogin(
		sessionID,
		fmt.Sprintf("http://127.0.0.1/oauth/callback?code=code-3&state=%s", state),
	)
	if err != nil {
		t.Fatalf("CompleteIamSsoLogin: %v", err)
	}
	if result.AuthRegion != "us-east-1" || result.ProfileRegionHint != "eu-central-1" || result.StartURL != startURL {
		t.Fatalf("unexpected routing metadata: %+v", result)
	}
}
