package web

// The Claude connector's sign-in (spec §10.4): the backend is its own OAuth 2.1 authorization server, for one
// person. Claude registers itself (RFC 7591), I approve it on a consent page behind the accounts page's login, and it
// gets tokens for the MCP endpoint (MCPPath). Only the tokens' hashes are kept.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

const (
	// MCPPath is the connector's address under BASE_URL (what is added on claude.ai: BASE_URL + MCPPath).
	MCPPath = "/mcp"

	accessLife  = time.Hour
	refreshLife = 90 * 24 * time.Hour
	codeLife    = 10 * time.Minute
	// maxUnsigned: registrations nobody signed in with yet; more are refused until PruneOAuth drops them (an hour).
	maxUnsigned = 20
	// reuseGrace: a refresh token used again this soon is a request sent twice (a retry, two of Claude's at once),
	// refused; later, a copy in someone else's hands, and the client is signed out.
	reuseGrace = 30 * time.Second
)

// claudeCallbacks are Claude's OAuth callbacks (Anthropic says the claude.com one may replace the other).
var claudeCallbacks = []string{"https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"}

// allowedRedirect: Claude's callbacks, and loopback addresses (an MCP client on my own computer, such as Claude
// Code; RFC 8252 §7.3). Any other site could have me approve a client that sends my tokens to it.
func allowedRedirect(u string) bool {
	if slices.Contains(claudeCallbacks, u) {
		return true
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "http" || p.Fragment != "" || p.User != nil {
		return false
	}
	h := p.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func (s *Server) resource() string { return s.BaseURL + MCPPath }

// oauthRoutes adds the sign-in and the MCP endpoint (bearer tokens only) to mux.
func (s *Server) oauthRoutes(mux *http.ServeMux, cop *http.CrossOriginProtection) {
	meta := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.resource(),
		AuthorizationServers:   []string{s.BaseURL},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Mon coffre et mon calendrier",
	})
	mux.Handle("GET /.well-known/oauth-protected-resource", meta)
	mux.Handle("GET /.well-known/oauth-protected-resource"+MCPPath, meta)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServerMeta)
	mux.HandleFunc("POST /register", s.register)
	mux.HandleFunc("GET /authorize", s.authorizePage)
	mux.HandleFunc("POST /authorize", s.authorize)
	mux.HandleFunc("POST /token", s.token)
	mcp := s.MCP
	if mcp == nil {
		mcp = http.NotFoundHandler()
	}
	mux.Handle(MCPPath, auth.RequireBearerToken(s.verifyToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.BaseURL + "/.well-known/oauth-protected-resource" + MCPPath,
	})(mcp))
	mux.Handle("POST /claude/{id}/revoke", s.auth(s.revoke))
	// Called by Claude's servers, not by my browser with its cookie: nothing for a cross-site form to abuse.
	for _, p := range []string{"POST /register", "POST /token", MCPPath} {
		cop.AddInsecureBypassPattern(p)
	}
}

// authServerMeta is RFC 8414's metadata (no jwks_uri: tokens are opaque, checked here).
func (s *Server) authServerMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.BaseURL,
		"authorization_endpoint":                         s.BaseURL + "/authorize",
		"token_endpoint":                                 s.BaseURL + "/token",
		"registration_endpoint":                          s.BaseURL + "/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_post", "client_secret_basic"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// --- registration (RFC 7591)

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		GrantTypes   []string `json:"grant_types"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "not a JSON client registration")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !allowedRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "this server only signs in Claude and clients on this computer")
			return
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant type "+g)
			return
		}
	}
	method := req.AuthMethod
	switch method {
	case "":
		method = "none"
	case "none", "client_secret_post", "client_secret_basic":
	default:
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	if err := s.Store.PruneOAuth(ctx, time.Now()); err != nil {
		s.oauthFail(w, err)
		return
	}
	list, err := s.Store.OAuthClients(ctx)
	if err != nil {
		s.oauthFail(w, err)
		return
	}
	unsigned := 0
	for _, c := range list {
		if !c.Signed && c.LastUsed.IsZero() {
			unsigned++
		}
	}
	if unsigned >= maxUnsigned {
		oauthError(w, http.StatusTooManyRequests, "invalid_client_metadata", "too many registrations, try again in an hour")
		return
	}
	name := strings.TrimSpace(req.ClientName)
	if len(name) > 100 {
		name = name[:100]
	}
	c := store.OAuthClient{ID: randomToken(), Name: name, RedirectURIs: req.RedirectURIs, Created: time.Now()}
	secret := ""
	if method != "none" {
		secret = randomToken()
		c.SecretHash = hashToken(secret)
	}
	if err := s.Store.AddOAuthClient(ctx, c); err != nil {
		s.oauthFail(w, err)
		return
	}
	s.Log.Info("connector: client registered", "client", c.ID[:8], "name", name)
	resp := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.Created.Unix(),
		"client_name":                name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": method,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, resp)
}

// --- authorization (the consent page)

// authRequest is an authorization request whose client and redirect URI are known good: errors after that go back
// to the client, the ones before are said here (never redirect to an address the client didn't register).
type authRequest struct {
	Client      store.OAuthClient
	RedirectURI string
	State       string
	Challenge   string
	Scope       string
	Resource    string
}

// readAuthRequest checks an authorization request. redirectErr is an OAuth error to send to the client's redirect;
// pageErr one to show here (bad client or redirect URI).
func (s *Server) readAuthRequest(ctx context.Context, v url.Values) (req authRequest, redirectErr, pageErr string) {
	c, err := s.Store.OAuthClient(ctx, v.Get("client_id"))
	if err != nil {
		return req, "", "Ce client n'est pas enregistré ici : reconnecte le connecteur depuis Claude."
	}
	req.Client = c
	req.RedirectURI = v.Get("redirect_uri")
	if req.RedirectURI == "" && len(c.RedirectURIs) == 1 {
		req.RedirectURI = c.RedirectURIs[0]
	}
	if !slices.Contains(c.RedirectURIs, req.RedirectURI) {
		return req, "", "L'adresse de retour de cette demande n'est pas celle que ce client a enregistrée."
	}
	req.State, req.Scope = v.Get("state"), v.Get("scope")
	req.Challenge, req.Resource = v.Get("code_challenge"), v.Get("resource")
	switch {
	case v.Get("response_type") != "code":
		return req, "unsupported_response_type", ""
	case req.Challenge == "" || v.Get("code_challenge_method") != "S256":
		return req, "invalid_request", ""
	case req.Resource != "" && strings.TrimSuffix(req.Resource, "/") != s.resource() && strings.TrimSuffix(req.Resource, "/") != s.BaseURL:
		return req, "invalid_target", ""
	}
	return req, "", ""
}

// back sends the browser to the client's redirect URI with these parameters (and the state, and the issuer).
func (req authRequest) back(w http.ResponseWriter, r *http.Request, issuer string, params url.Values) {
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

type consentPage struct {
	Client   string // its name
	Host     string // where it sends me back
	Hidden   map[string]string
	Resource string
}

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req, redirectErr, pageErr := s.readAuthRequest(r.Context(), q)
	switch {
	case pageErr != "":
		s.render(w, http.StatusBadRequest, "error", page{Error: pageErr})
		return
	case redirectErr != "":
		req.back(w, r, s.BaseURL, url.Values{"error": {redirectErr}})
		return
	case !s.loggedIn(r):
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	hidden := map[string]string{}
	for _, k := range []string{"client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "scope",
		"resource", "response_type"} {
		if v := q.Get(k); v != "" {
			hidden[k] = v
		}
	}
	u, _ := url.Parse(req.RedirectURI)
	name := req.Client.Name
	if name == "" {
		name = "Un client sans nom"
	}
	// The form's answer redirects to the client: a loopback client is plain http, which form-action must allow.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; "+
		"form-action 'self' https: http://localhost:* http://127.0.0.1:* http://[::1]:*; frame-ancestors 'none'")
	s.renderAny(w, http.StatusOK, "consent", consentPage{Client: name, Host: u.Host, Hidden: hidden, Resource: s.resource()})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		s.render(w, http.StatusUnauthorized, "error", page{Error: "Ta session a expiré : reconnecte-toi, puis recommence depuis Claude."})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.problem(w, "Demande illisible.")
		return
	}
	req, redirectErr, pageErr := s.readAuthRequest(r.Context(), r.PostForm)
	switch {
	case pageErr != "":
		s.render(w, http.StatusBadRequest, "error", page{Error: pageErr})
		return
	case redirectErr != "":
		req.back(w, r, s.BaseURL, url.Values{"error": {redirectErr}})
		return
	case r.PostFormValue("decision") != "allow":
		req.back(w, r, s.BaseURL, url.Values{"error": {"access_denied"}})
		return
	}
	code := randomToken()
	s.mu.Lock()
	for k, c := range s.codes {
		if time.Now().After(c.expires) {
			delete(s.codes, k)
		}
	}
	s.codes[hashToken(code)] = authCode{client: req.Client.ID, redirect: req.RedirectURI, challenge: req.Challenge,
		expires: time.Now().Add(codeLife)}
	s.mu.Unlock()
	s.Log.Info("connector: client approved", "client", req.Client.ID[:8], "name", req.Client.Name)
	req.back(w, r, s.BaseURL, url.Values{"code": {code}})
}

// authCode is an authorization code given and not exchanged yet (kept by its hash, in memory: it lives minutes).
type authCode struct {
	client, redirect, challenge string
	expires                     time.Time
}

func (s *Server) takeCode(code string) (authCode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[hashToken(code)]
	delete(s.codes, hashToken(code))
	return c, ok && time.Now().Before(c.expires)
}

// --- tokens

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "unreadable form")
		return
	}
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	c, err := s.Store.OAuthClient(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	} else if err != nil {
		s.oauthFail(w, err)
		return
	}
	if c.SecretHash != "" && subtle.ConstantTimeCompare([]byte(hashToken(secret)), []byte(c.SecretHash)) != 1 {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "wrong client secret")
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		code, ok := s.takeCode(r.PostFormValue("code"))
		if !ok || code.client != c.ID {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown, used or expired code")
			return
		}
		if ru := r.PostFormValue("redirect_uri"); ru != "" && ru != code.redirect {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri differs from the authorization request's")
			return
		}
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(code.challenge)) != 1 {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match the code challenge")
			return
		}
	case "refresh_token":
		// A refresh token serves once. Used again, it was copied (security review, 2026-11-05: a stolen one kept
		// working for 90 days, and the client it was stolen from was only signed out at its next refresh): every
		// token of the client goes, the thief's and the client's alike, and it shows signed out on the page.
		now := time.Now()
		t, err := s.Store.UseRefreshToken(ctx, hashToken(r.PostFormValue("refresh_token")), c.ID, now)
		switch {
		case errors.Is(err, store.ErrUsed):
			if now.Sub(t.Used) > reuseGrace {
				s.Log.Warn("connector: a refresh token was used twice, its client is signed out", "client", c.ID,
					"from", clientAddr(r))
				if err := s.Store.DeleteOAuthTokens(ctx, c.ID); err != nil {
					s.oauthFail(w, err)
					return
				}
			}
			oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown, used or expired refresh token")
			return
		case errors.Is(err, store.ErrNotFound):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown, used or expired refresh token")
			return
		case err != nil:
			s.oauthFail(w, err)
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
		return
	}
	access, refresh := randomToken(), randomToken()
	now := time.Now()
	for _, t := range []store.OAuthToken{
		{Hash: hashToken(access), Kind: "access", ClientID: c.ID, Expires: now.Add(accessLife)},
		{Hash: hashToken(refresh), Kind: "refresh", ClientID: c.ID, Expires: now.Add(refreshLife)},
	} {
		if err := s.Store.AddOAuthToken(ctx, t); err != nil {
			s.oauthFail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessLife.Seconds()),
		"refresh_token": refresh,
	})
}

// verifyToken is the MCP endpoint's check: an access token given here that hasn't expired.
func (s *Server) verifyToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	t, err := s.Store.OAuthToken(ctx, hashToken(token), "access", time.Now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, auth.ErrInvalidToken
	} else if err != nil {
		s.Log.Error("connector: token check", "err", err) // the answer's body says only that it failed
		return nil, errors.New("server error")
	}
	s.touch(ctx, t.ClientID)
	return &auth.TokenInfo{Expiration: t.Expires, UserID: t.ClientID}, nil
}

// touch records when a client last called, at most once a minute.
func (s *Server) touch(ctx context.Context, client string) {
	now := time.Now()
	s.mu.Lock()
	last := s.touched[client]
	if now.Sub(last) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.touched[client] = now
	s.mu.Unlock()
	if err := s.Store.TouchOAuthClient(ctx, client, now); err != nil {
		s.Log.Warn("connector: last use not saved", "err", err)
	}
}

// revoke signs a client out for good (the accounts page's Déconnecter).
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteOAuthClient(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("connector: client signed out from the accounts page")
	http.Redirect(w, r, "/?ok=claude-revoked#claude", http.StatusSeeOther)
}

// claudeRows are the clients I signed in (registrations nobody signed in with go in an hour).
func (s *Server) claudeRows(ctx context.Context) ([]claudeRow, error) {
	list, err := s.Store.OAuthClients(ctx)
	var rows []claudeRow
	for _, c := range list {
		if !c.Signed && c.LastUsed.IsZero() {
			continue
		}
		r := claudeRow{ID: c.ID, Name: c.Name, Signed: c.Signed, LastUsedText: "jamais utilisé"}
		if r.Name == "" {
			r.Name = "Client sans nom"
		}
		if !c.LastUsed.IsZero() {
			r.LastUsedText = "utilisé le " + c.LastUsed.In(s.Engine.Loc).Format("02/01/2006 à 15:04")
		}
		rows = append(rows, r)
	}
	return rows, err
}

// --- helpers

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="token"`)
	}
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (s *Server) oauthFail(w http.ResponseWriter, err error) {
	s.Log.Error("connector sign-in", "err", err)
	oauthError(w, http.StatusInternalServerError, "server_error", "server error")
}
