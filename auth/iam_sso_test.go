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
	accessToken, refreshToken, _, _, region, _, err := CompleteIamSsoLogin(sessionID, callbackURL)
	if err != nil {
		t.Fatalf("CompleteIamSsoLogin: %v", err)
	}
	if accessToken != "access-2" || refreshToken != "refresh-2" || region != "us-east-1" {
		t.Fatalf("unexpected completion result: access=%q refresh=%q region=%q", accessToken, refreshToken, region)
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

	_, _, _, _, _, _, err := CompleteIamSsoLogin(sessionID, "http://127.0.0.1/oauth/callback")
	if err == nil {
		t.Fatal("expected expired session error")
	}
	if session.ClientID != "" || session.ClientSecret != "" || session.CodeVerifier != "" || session.State != "" {
		t.Fatalf("expired session retained sensitive fields: %+v", session)
	}
}
