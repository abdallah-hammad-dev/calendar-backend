package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/connector"
)

const claudeCallback = "https://claude.ai/api/mcp/auth_callback"

// echoMCP stands for the connector: it says which client called.
var echoMCP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "client "+auth.TokenInfoFromContext(r.Context()).UserID)
})

func withMCP(s *Server) { s.MCP = echoMCP }

func (e env) postJSON(t *testing.T, path string, body any, header ...string) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// tokenCall posts to /token as a server does (no cookie, cross-site).
func (e env) tokenCall(t *testing.T, form url.Values, basic ...string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if len(basic) == 2 {
		req.SetBasicAuth(basic[0], basic[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (e env) callMCP(t *testing.T, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+MCPPath, strings.NewReader(`{}`))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeQuery(client, verifier, state string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {claudeCallback},
		"code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"}, "state": {state},
		"resource": {"http://cal.test/mcp"}}
}

// approve answers the consent page, as I do, and returns where the browser goes back.
func (e env) approve(t *testing.T, q url.Values, decision string) url.Values {
	t.Helper()
	form := url.Values{"decision": {decision}}
	for k, v := range q {
		form[k] = v
	}
	resp := e.post(t, "/authorize", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize: %s", resp.Status)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	if got := back.Scheme + "://" + back.Host + back.Path; got != claudeCallback {
		t.Fatalf("sent back to %s", got)
	}
	return back.Query()
}

// The connector's whole sign-in, as Claude does it: discovery, registration, consent behind the login, code for
// tokens with PKCE, the MCP endpoint with the token, refresh, then signed out from the accounts page.
func TestConnectorSignIn(t *testing.T) {
	e := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", nil, withMCP)

	resp, body := e.callMCP(t, "")
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="http://cal.test/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("no token: %s %q", resp.Status, resp.Header.Get("WWW-Authenticate"))
	}
	var prm, asm map[string]any
	_, body = e.get(t, "/.well-known/oauth-protected-resource/mcp")
	json.Unmarshal([]byte(body), &prm)
	if prm["resource"] != "http://cal.test/mcp" || prm["authorization_servers"].([]any)[0] != "http://cal.test" {
		t.Fatalf("resource metadata: %s", body)
	}
	_, body = e.get(t, "/.well-known/oauth-authorization-server")
	json.Unmarshal([]byte(body), &asm)
	if asm["registration_endpoint"] != "http://cal.test/register" || asm["code_challenge_methods_supported"].([]any)[0] != "S256" {
		t.Fatalf("server metadata: %s", body)
	}

	// Registration: only Claude's callbacks and loopback addresses.
	for _, bad := range []string{"https://evil.test/cb", "http://cal.test.evil/cb", "https://claude.ai/api/mcp/auth_callback/x"} {
		if resp, _ := e.postJSON(t, "/register", map[string]any{"redirect_uris": []string{bad}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("registered %s: %s", bad, resp.Status)
		}
	}
	if resp, _ := e.postJSON(t, "/register", map[string]any{"redirect_uris": []string{"http://127.0.0.1:33418/callback"}},
		"Sec-Fetch-Site", "cross-site"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("loopback client: %s", resp.Status)
	}
	resp, reg := e.postJSON(t, "/register", map[string]any{"redirect_uris": []string{claudeCallback}, "client_name": "Claude",
		"token_endpoint_auth_method": "none"})
	client, _ := reg["client_id"].(string)
	if resp.StatusCode != http.StatusCreated || client == "" || reg["client_secret"] != nil {
		t.Fatalf("register: %s %v", resp.Status, reg)
	}

	// Not logged in: the login first, and back to the consent page after it.
	q := authorizeQuery(client, "the-verifier-of-at-least-43-characters-long-ok", "st4te")
	resp, _ = e.get(t, "/authorize?"+q.Encode())
	next := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(next, "/login?next=") {
		t.Fatalf("authorize logged out: %s %s", resp.Status, next)
	}
	loginURL, _ := url.Parse(next)
	_, body = e.get(t, next)
	if !strings.Contains(body, `name="next"`) {
		t.Fatal("the login page forgets where it was sent from")
	}
	resp = e.post(t, "/login", url.Values{"user": {"abd"}, "password": {"secret"}, "next": {loginURL.Query().Get("next")}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/authorize?"+q.Encode() {
		t.Fatalf("login with next: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	resp, body = e.get(t, "/authorize?"+q.Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Autoriser Claude") || !strings.Contains(body, "claude.ai") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "http://localhost:*") {
		t.Fatalf("consent page: %s\n%s", resp.Status, body)
	}

	// Refused: Claude is told, with its state.
	if back := e.approve(t, q, "deny"); back.Get("error") != "access_denied" || back.Get("state") != "st4te" || back.Get("code") != "" {
		t.Fatalf("denied: %v", back)
	}

	// A code serves once, and only with its verifier.
	back := e.approve(t, q, "allow")
	if back.Get("state") != "st4te" || back.Get("iss") != "http://cal.test" || back.Get("code") == "" {
		t.Fatalf("approved: %v", back)
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {back.Get("code")}, "client_id": {client},
		"redirect_uri": {claudeCallback}, "code_verifier": {"another-verifier"}}
	if resp, out := e.tokenCall(t, exchange); resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %s %v", resp.Status, out)
	}
	back = e.approve(t, q, "allow")
	exchange.Set("code", back.Get("code"))
	exchange.Set("code_verifier", "the-verifier-of-at-least-43-characters-long-ok")
	resp, tok := e.tokenCall(t, exchange)
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if resp.StatusCode != http.StatusOK || access == "" || refresh == "" || tok["token_type"] != "Bearer" {
		t.Fatalf("token: %s %v", resp.Status, tok)
	}
	if resp, _ := e.tokenCall(t, exchange); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code used twice: %s", resp.Status)
	}

	if resp, body := e.callMCP(t, access); resp.StatusCode != http.StatusOK || body != "client "+client {
		t.Fatalf("mcp: %s %s", resp.Status, body)
	}
	if resp, _ := e.callMCP(t, "made-up"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("made-up token: %s", resp.Status)
	}

	// Refresh: new tokens, the old refresh token is spent.
	again := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {client}}
	resp, tok2 := e.tokenCall(t, again)
	if resp.StatusCode != http.StatusOK || tok2["access_token"] == access || tok2["refresh_token"] == refresh {
		t.Fatalf("refresh: %s %v", resp.Status, tok2)
	}
	if resp, _ := e.tokenCall(t, again); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("refresh token used twice: %s", resp.Status)
	}
	if resp, _ := e.callMCP(t, tok2["access_token"].(string)); resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp after refresh: %s", resp.Status)
	}

	// The accounts page lists it; Déconnecter signs it out for good.
	list, err := e.store.OAuthClients(context.Background())
	if err != nil || len(list) != 2 || list[0].ID != client || !list[0].Signed || list[0].LastUsed.IsZero() {
		t.Fatalf("clients: %v %v", list, err)
	}
	_, body = e.get(t, "/")
	if !strings.Contains(body, "/claude/"+client+"/revoke") {
		t.Fatal("the accounts page doesn't list the connector")
	}
	if resp := e.post(t, "/claude/"+client+"/revoke", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke: %s", resp.Status)
	}
	if resp, _ := e.callMCP(t, tok2["access_token"].(string)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mcp after revoke: %s", resp.Status)
	}
}

// What a request that can't be trusted gets: never a redirect to an address the client didn't register.
func TestConnectorBadRequests(t *testing.T) {
	e := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", nil, withMCP)
	e.login(t)
	_, reg := e.postJSON(t, "/register", map[string]any{"redirect_uris": []string{claudeCallback}})
	client := reg["client_id"].(string)

	q := authorizeQuery(client, "v", "s")
	q.Set("redirect_uri", "https://evil.test/cb")
	if resp, _ := e.get(t, "/authorize?"+q.Encode()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("another redirect URI: %s", resp.Status)
	}
	q = authorizeQuery("unknown", "v", "s")
	if resp, _ := e.get(t, "/authorize?"+q.Encode()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown client: %s", resp.Status)
	}
	q = authorizeQuery(client, "v", "s")
	q.Del("code_challenge")
	resp, _ := e.get(t, "/authorize?"+q.Encode())
	back, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || back.Query().Get("error") != "invalid_request" {
		t.Fatalf("no PKCE: %s %s", resp.Status, back)
	}
	q = authorizeQuery(client, "v", "s")
	q.Set("resource", "https://other.test/mcp")
	resp, _ = e.get(t, "/authorize?"+q.Encode())
	if back, _ := url.Parse(resp.Header.Get("Location")); back.Query().Get("error") != "invalid_target" {
		t.Fatalf("another resource: %s", back)
	}

	// The login goes on only to the consent page.
	for _, next := range []string{"https://evil.test/", "//evil.test/", "/accounts/1/remove"} {
		resp := e.post(t, "/login", url.Values{"user": {"abd"}, "password": {"secret"}, "next": {next}})
		if resp.Header.Get("Location") != "/" {
			t.Errorf("next %s: %s", next, resp.Header.Get("Location"))
		}
	}

	// A confidential client: its secret, in the form or in Basic.
	_, reg = e.postJSON(t, "/register", map[string]any{"redirect_uris": []string{claudeCallback},
		"token_endpoint_auth_method": "client_secret_basic"})
	conf, secret := reg["client_id"].(string), reg["client_secret"].(string)
	verifier := "a-verifier"
	code := e.approve(t, authorizeQuery(conf, verifier, "s"), "allow").Get("code")
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}}
	if resp, out := e.tokenCall(t, form, conf, "wrong"); resp.StatusCode != http.StatusUnauthorized || out["error"] != "invalid_client" {
		t.Fatalf("wrong secret: %s %v", resp.Status, out)
	}
	code = e.approve(t, authorizeQuery(conf, verifier, "s"), "allow").Get("code")
	form.Set("code", code)
	if resp, out := e.tokenCall(t, form, conf, secret); resp.StatusCode != http.StatusOK {
		t.Fatalf("basic: %s %v", resp.Status, out)
	}
	// Another client's code isn't mine.
	code = e.approve(t, authorizeQuery(conf, verifier, "s"), "allow").Get("code")
	form = url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "client_id": {client}}
	if resp, _ := e.tokenCall(t, form); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("another client's code: %s", resp.Status)
	}

	// Logged out, the consent form can't be answered.
	e.post(t, "/logout", nil)
	form = url.Values{"decision": {"allow"}}
	for k, v := range authorizeQuery(client, verifier, "s") {
		form[k] = v
	}
	if resp := e.post(t, "/authorize", form); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("authorize logged out: %s", resp.Status)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// The real connector behind the sign-in, over streamable HTTP as Claude calls it: its tools are listed.
func TestConnectorOverHTTP(t *testing.T) {
	conn := &connector.Connector{Loc: time.UTC, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		TasksFolder: "Tasks", EventsFolder: "Calendar", EventDateFormat: "DD-MM-YYYY"}
	e := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", nil, func(s *Server) { s.MCP = conn.Handler() })
	e.login(t)
	_, reg := e.postJSON(t, "/register", map[string]any{"redirect_uris": []string{claudeCallback}, "client_name": "Claude"})
	client := reg["client_id"].(string)
	code := e.approve(t, authorizeQuery(client, "v", "s"), "allow").Get("code")
	_, tok := e.tokenCall(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {"v"},
		"client_id": {client}})
	ctx := context.Background()
	tr := &mcp.StreamableClientTransport{Endpoint: e.srv.URL + MCPPath,
		HTTPClient: &http.Client{Transport: bearer{tok["access_token"].(string)}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-test", Version: "1"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if !strings.Contains(cs.InitializeResult().Instructions, "task_create") {
		t.Error("no instructions")
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 21 {
		t.Fatalf("tools: %v %v", tools, err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "vault_list", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "isn't mirrored") {
		t.Fatalf("vault_list without a vault: %v %v", res, err)
	}
}
