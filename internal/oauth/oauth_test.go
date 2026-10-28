package oauth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestAuthURL(t *testing.T) {
	p := Google("id", "secret", "https://cal.example/oauth/callback")
	u, err := url.Parse(p.AuthURL("st", oauth2.GenerateVerifier()))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, want := range map[string]string{"state": "st", "access_type": "offline", "prompt": "consent",
		"code_challenge_method": "S256", "redirect_uri": "https://cal.example/oauth/callback"} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.Contains(q.Get("scope"), "calendar.events") || q.Get("code_challenge") == "" {
		t.Fatal(q)
	}
	m, _ := url.Parse(Microsoft("id", "secret", "common", "https://x/cb").AuthURL("st", "v"))
	if m.Host != "login.microsoftonline.com" || !strings.HasPrefix(m.Path, "/common/") ||
		!strings.Contains(m.Query().Get("scope"), "offline_access") {
		t.Fatal(m)
	}
}

// A token endpoint that rotates the refresh token at each refresh, as Microsoft does.
func tokenServer(t *testing.T, refreshes *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			if r.Form.Get("refresh_token") == "revoked" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70008: expired"}`))
				return
			}
			*refreshes++
			w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","token_type":"Bearer","expires_in":3600}`))
		case "authorization_code":
			if r.Form.Get("code_verifier") == "" {
				t.Error("no PKCE verifier")
			}
			idToken := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"me@gmail.com","email_verified":true}`)) + ".s"
			w.Write([]byte(`{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":3600,"id_token":"` + idToken + `"}`))
		}
	}))
}

func TestClientSavesRefreshedTokens(t *testing.T) {
	var refreshes int
	ts := tokenServer(t, &refreshes)
	defer ts.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer api.Close()

	p := Microsoft("id", "secret", "common", "https://x/cb")
	p.Config.Endpoint = oauth2.Endpoint{TokenURL: ts.URL, AuthStyle: oauth2.AuthStyleInParams}
	var saved []*oauth2.Token
	save := func(tok *oauth2.Token) error { saved = append(saved, tok); return nil }
	expired := &oauth2.Token{AccessToken: "access-1", RefreshToken: "refresh-1", Expiry: time.Now().Add(-time.Minute)}
	hc := p.Client(context.Background(), expired, save)
	for range 2 {
		resp, err := hc.Get(api.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if refreshes != 1 || len(saved) != 1 || saved[0].RefreshToken != "refresh-2" {
		t.Fatalf("refreshes %d, saved %+v", refreshes, saved)
	}

	// A token still valid is used as is, nothing saved.
	saved = nil
	valid := &oauth2.Token{AccessToken: "access-1", RefreshToken: "refresh-1", Expiry: time.Now().Add(time.Hour)}
	resp, err := p.Client(context.Background(), valid, save).Get(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(saved) != 0 {
		t.Fatal("saved an unchanged token")
	}

	// A save that fails fails the request: the rotated refresh token would be lost.
	failing := func(*oauth2.Token) error { return errors.New("disk full") }
	if _, err := p.Client(context.Background(), expired, failing).Get(api.URL); err == nil {
		t.Fatal("request went through without the token saved")
	}

	// Revoked: sign in again.
	revoked := &oauth2.Token{AccessToken: "x", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Minute)}
	_, err = p.Client(context.Background(), revoked, save).Get(api.URL)
	if !SignInAgain(err) {
		t.Fatalf("not seen as a sign-in to redo: %v", err)
	}
	if SignInAgain(errors.New("network down")) {
		t.Fatal("any error seen as a sign-in to redo")
	}
}

func TestExchangeAndClaims(t *testing.T) {
	var refreshes int
	ts := tokenServer(t, &refreshes)
	defer ts.Close()
	p := Google("id", "secret", "https://x/cb")
	p.Config.Endpoint = oauth2.Endpoint{TokenURL: ts.URL, AuthStyle: oauth2.AuthStyleInParams}
	tok, err := p.Exchange(context.Background(), "code", oauth2.GenerateVerifier())
	if err != nil {
		t.Fatal(err)
	}
	c, err := IDClaims(tok)
	if err != nil || c.Email != "me@gmail.com" || c.EmailVerified == nil || !*c.EmailVerified {
		t.Fatalf("%+v %v", c, err)
	}

	s, err := Load(Stored{Token: tok}.Encode())
	if err != nil || s.Token.RefreshToken != "r" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := Load(`{}`); err == nil {
		t.Fatal("an account without a token loaded")
	}
}
