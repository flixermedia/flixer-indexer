package runner

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
)

func TestRunBuiltGithubOAuthWASIProvider(t *testing.T) {
	module := os.Getenv("FLIXER_TEST_GITHUB_OAUTH_WASM")
	if module == "" {
		t.Skip("set FLIXER_TEST_GITHUB_OAUTH_WASM to a compiled GitHub OAuth example")
	}
	input := []byte(`{"apiVersion":1,"method":"auth-start","settings":{"clientId":"test-client"},"request":{"body":{"state":"test-state","codeChallenge":"test-challenge","redirectUri":"https://flixer.example/auth/provider-callback"}}}`)
	output, err := newTestRunner(t).Run(context.Background(), module, input)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			AuthorizationURL string `json:"authorizationUrl"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	authorization, err := url.Parse(response.Result.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if authorization.Host != "github.com" || authorization.Path != "/login/oauth/authorize" || authorization.Query().Get("code_challenge_method") != "S256" || authorization.Query().Get("state") != "test-state" {
		t.Fatalf("unexpected authorization URL: %s", response.Result.AuthorizationURL)
	}
}

func TestRunBuiltConfigurableOAuthWASIProvider(t *testing.T) {
	module := os.Getenv("FLIXER_TEST_GITHUB_OAUTH_WASM")
	if module == "" {
		t.Skip("set FLIXER_TEST_GITHUB_OAUTH_WASM to a compiled OAuth provider")
	}
	runner := newTestRunner(t)
	settings := map[string]any{
		"clientId": "test-client", "clientSecret": "test-secret",
		"authorizationUrl": "https://login.example.com/authorize", "tokenUrl": "https://login.example.com/token",
		"userInfoUrl": "https://api.example.com/user", "issuer": "https://login.example.com/", "subjectField": "sub", "scope": "profile",
	}
	input := map[string]any{"method": "auth-start", "settings": settings, "request": map[string]any{"body": map[string]any{"state": "state", "codeChallenge": "challenge", "redirectUri": "https://flixer.example/auth/provider-callback"}}}
	run := func() map[string]any {
		payload, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		output, err := runner.Run(context.Background(), module, payload)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	start := run()["result"].(map[string]any)
	authorization, err := url.Parse(start["authorizationUrl"].(string))
	if err != nil || authorization.Host != "login.example.com" || authorization.Query().Get("scope") != "profile" {
		t.Fatalf("unexpected configured authorization URL: %v", authorization)
	}
	input["method"] = "auth-complete"
	input["request"] = map[string]any{"body": map[string]any{"code": "code", "codeVerifier": "verifier", "redirectUri": "https://flixer.example/auth/provider-callback"}}
	if call := run()["hostCalls"].([]any)[0].(map[string]any); call["url"] != "https://login.example.com/token" {
		t.Fatalf("unexpected token endpoint: %v", call["url"])
	}
	input["continuation"] = map[string]any{"step": "token"}
	input["hostResults"] = []any{map[string]any{"id": "token", "ok": true, "body": `{"access_token":"token"}`}}
	if call := run()["hostCalls"].([]any)[0].(map[string]any); call["url"] != "https://api.example.com/user" {
		t.Fatalf("unexpected user-info endpoint: %v", call["url"])
	}
	input["continuation"] = map[string]any{"step": "user"}
	input["hostResults"] = []any{map[string]any{"id": "user", "ok": true, "body": `{"sub":"stable-subject"}`}}
	identity := run()["result"].(map[string]any)
	if identity["authenticated"] != true || identity["issuer"] != "https://login.example.com/" || identity["subject"] != "stable-subject" {
		t.Fatalf("unexpected identity: %#v", identity)
	}
}