package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
)

type IamSsoSession struct {
	ClientID          string
	ClientSecret      string
	CodeVerifier      string
	State             string
	AuthRegion        string
	ProfileRegionHint string
	StartURL          string
	RedirectURI       string
	ExpiresAt         time.Time
	timer             *time.Timer
}

var (
	sessions   = make(map[string]*IamSsoSession)
	sessionsMu sync.RWMutex
)

var scopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
	"codewhisperer:transformations",
	"codewhisperer:taskassist",
}

var iamOIDCBaseURL = func(region string) string {
	return fmt.Sprintf("https://oidc.%s.amazonaws.com", region)
}

// StartIamSsoLogin 发起 IAM SSO 登录
func StartIamSsoLogin(startURL, authRegion, profileRegionHint string) (sessionID, authorizeURL string, expiresIn int, err error) {
	if authRegion == "" {
		authRegion = "us-east-1"
	}

	oidcBase := iamOIDCBaseURL(authRegion)
	redirectURI := "http://127.0.0.1/oauth/callback"

	// 1. 注册 OIDC 客户端
	clientID, clientSecret, err := registerOIDCClient(oidcBase, startURL, redirectURI)
	if err != nil {
		return "", "", 0, fmt.Errorf("注册客户端失败: %w", err)
	}

	// 2. 生成 PKCE
	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)
	state := uuid.New().String()

	// 3. 构建授权 URL
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scopes", joinScopes())
	params.Set("state", state)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")

	authorizeURL = fmt.Sprintf("%s/authorize?%s", oidcBase, params.Encode())

	// 4. 保存会话
	sessionID = uuid.New().String()
	session := &IamSsoSession{
		ClientID:          clientID,
		ClientSecret:      clientSecret,
		CodeVerifier:      codeVerifier,
		State:             state,
		AuthRegion:        authRegion,
		ProfileRegionHint: profileRegionHint,
		StartURL:          startURL,
		RedirectURI:       redirectURI,
		ExpiresAt:         time.Now().Add(10 * time.Minute),
	}

	sessionsMu.Lock()
	sessions[sessionID] = session
	sessionsMu.Unlock()

	session.timer = time.AfterFunc(10*time.Minute, func() {
		CancelIamSsoLogin(sessionID)
	})

	return sessionID, authorizeURL, 600, nil
}

// CompleteIamSsoLogin 完成 IAM SSO 登录
func CompleteIamSsoLogin(sessionID, callbackUrl string) (accessToken, refreshToken, clientID, clientSecret, region string, expiresIn int, err error) {
	sessionsMu.RLock()
	session, ok := sessions[sessionID]
	sessionsMu.RUnlock()

	if !ok {
		return "", "", "", "", "", 0, fmt.Errorf("会话不存在或已过期")
	}

	if time.Now().After(session.ExpiresAt) {
		CancelIamSsoLogin(sessionID)
		return "", "", "", "", "", 0, fmt.Errorf("会话已过期")
	}

	// 解析回调 URL
	parsedUrl, err := url.Parse(callbackUrl)
	if err != nil {
		return "", "", "", "", "", 0, fmt.Errorf("无效的回调 URL")
	}

	code := parsedUrl.Query().Get("code")
	state := parsedUrl.Query().Get("state")
	errorParam := parsedUrl.Query().Get("error")

	if errorParam != "" {
		return "", "", "", "", "", 0, fmt.Errorf("授权失败: %s", errorParam)
	}

	if state != session.State {
		return "", "", "", "", "", 0, fmt.Errorf("状态不匹配，可能存在安全风险")
	}

	if code == "" {
		return "", "", "", "", "", 0, fmt.Errorf("未收到授权码")
	}

	// 用 code 换取 token
	oidcBase := iamOIDCBaseURL(session.AuthRegion)
	accessToken, refreshToken, expiresIn, err = exchangeToken(
		oidcBase,
		session.ClientID,
		session.ClientSecret,
		code,
		session.CodeVerifier,
		session.RedirectURI,
	)
	if err != nil {
		return "", "", "", "", "", 0, err
	}

	clientID = session.ClientID
	clientSecret = session.ClientSecret
	region = session.AuthRegion
	CancelIamSsoLogin(sessionID)

	return accessToken, refreshToken, clientID, clientSecret, region, expiresIn, nil
}

func CancelIamSsoLogin(sessionID string) {
	sessionsMu.Lock()
	session, ok := sessions[sessionID]
	if ok {
		delete(sessions, sessionID)
	}
	sessionsMu.Unlock()
	if !ok {
		return
	}
	if session.timer != nil {
		session.timer.Stop()
	}
	session.ClientID = ""
	session.ClientSecret = ""
	session.CodeVerifier = ""
	session.State = ""
}

func registerOIDCClient(oidcBase, startUrl, redirectUri string) (clientID, clientSecret string, err error) {
	payload := map[string]interface{}{
		"clientName":   "Kiro",
		"clientType":   "public",
		"scopes":       scopes,
		"grantTypes":   []string{"authorization_code", "refresh_token"},
		"redirectUris": []string{redirectUri},
		"issuerUrl":    startUrl,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", oidcBase+"/client/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", err
	}

	return result.ClientID, result.ClientSecret, nil
}

func exchangeToken(oidcBase, clientID, clientSecret, code, codeVerifier, redirectUri string) (accessToken, refreshToken string, expiresIn int, err error) {
	payload := map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"grantType":    "authorization_code",
		"redirectUri":  redirectUri,
		"code":         code,
		"codeVerifier": codeVerifier,
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", oidcBase+"/token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", 0, err
	}

	return result.AccessToken, result.RefreshToken, result.ExpiresIn, nil
}

func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func joinScopes() string {
	result := ""
	for i, s := range scopes {
		if i > 0 {
			result += ","
		}
		result += s
	}
	return result
}
