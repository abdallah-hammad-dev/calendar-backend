// Package oauth signs into Google and Microsoft accounts (authorization code with PKCE, spec §4, §7) and keeps their
// tokens fresh. Every new token is handed back to be saved: Microsoft gives a new refresh token at each refresh.
package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

// Provider is one kind of account that is signed into.
type Provider struct {
	Kind   string // the account kind: "google", "microsoft"
	Config *oauth2.Config
	extra  []oauth2.AuthCodeOption
}

// Google asks to read and edit events (mirror, answers, and part 2's Meet links: one sign-in, spec §4) and the
// account's address. A refresh token comes only with access_type=offline, and again only with prompt=consent.
func Google(clientID, clientSecret, redirectURL string) *Provider {
	return &Provider{
		Kind: "google",
		Config: &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: endpoints.Google,
			Scopes:   []string{"openid", "email", "https://www.googleapis.com/auth/calendar.events"}},
		extra: []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent")},
	}
}

// Microsoft signs into personal and any organization's accounts with tenant "common" (spec §4). offline_access
// gives the refresh token.
func Microsoft(clientID, clientSecret, tenant, redirectURL string) *Provider {
	return &Provider{
		Kind: "microsoft",
		Config: &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: endpoints.AzureAD(tenant),
			Scopes:   []string{"openid", "email", "offline_access", "https://graph.microsoft.com/Calendars.ReadWrite"}},
		extra: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("prompt", "select_account")},
	}
}

// AuthURL is where the browser signs in. state ties the callback to this attempt; verifier is PKCE's secret, kept
// by the caller until the callback.
func (p *Provider) AuthURL(state, verifier string) string {
	return p.Config.AuthCodeURL(state, append(p.extra, oauth2.S256ChallengeOption(verifier))...)
}

// Exchange trades the callback's code for the account's first token.
func (p *Provider) Exchange(ctx context.Context, code, verifier string) (*oauth2.Token, error) {
	tok, err := p.Config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("oauth: no refresh token given: the backend couldn't stay signed in")
	}
	return tok, nil
}

// Client is an HTTP client signed in with tok, which it refreshes when needed. save gets every new token (a
// failure to save fails the request: a rotated refresh token that isn't kept would sign the account out).
func (p *Provider) Client(ctx context.Context, tok *oauth2.Token, save func(*oauth2.Token) error) *http.Client {
	src := &savingSource{base: p.Config.TokenSource(ctx, tok), last: tok, save: save}
	return oauth2.NewClient(ctx, src)
}

type savingSource struct {
	base oauth2.TokenSource
	save func(*oauth2.Token) error

	mu   sync.Mutex
	last *oauth2.Token
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	tok, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil || tok.AccessToken != s.last.AccessToken || tok.RefreshToken != s.last.RefreshToken {
		if err := s.save(tok); err != nil {
			return nil, fmt.Errorf("oauth: saving the refreshed token: %w", err)
		}
		s.last = tok
	}
	return tok, nil
}

// SignInAgain tells whether err means the account's sign-in is over (revoked, expired, a password changed, a new
// consent needed): only signing in again on the accounts page fixes it.
func SignInAgain(err error) bool {
	var re *oauth2.RetrieveError
	return errors.As(err, &re) && (re.ErrorCode == "invalid_grant" || re.ErrorCode == "interaction_required" ||
		re.ErrorCode == "consent_required")
}

// Claims is what the ID token says about the account signed into.
type Claims struct {
	Email             string `json:"email"`
	EmailVerified     *bool  `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"` // Microsoft: the sign-in name, usually the address
	Name              string `json:"name"`
}

// IDClaims reads the ID token that came with tok. It came straight from the provider's token endpoint over TLS,
// so its signature isn't checked again (OpenID Connect Core §3.1.3.7).
func IDClaims(tok *oauth2.Token) (Claims, error) {
	raw, _ := tok.Extra("id_token").(string)
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("oauth: no ID token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Claims{}, fmt.Errorf("oauth: ID token: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, fmt.Errorf("oauth: ID token: %w", err)
	}
	return c, nil
}

// Stored is the sign-in as kept in an account's config.
type Stored struct {
	Token *oauth2.Token `json:"token"`
}

// Load reads an account's config. An account without a token must be signed into again.
func Load(config string) (Stored, error) {
	var s Stored
	if err := json.Unmarshal([]byte(config), &s); err != nil {
		return s, fmt.Errorf("oauth: bad account config: %w", err)
	}
	if s.Token == nil || s.Token.RefreshToken == "" {
		return s, errors.New("oauth: the account has no sign-in: sign in again on the accounts page")
	}
	return s, nil
}

func (s Stored) Encode() string {
	b, _ := json.Marshal(s)
	return string(b)
}
