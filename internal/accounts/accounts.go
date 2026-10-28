// Package accounts knows each kind of account: how it is signed into (OAuth for Google and Microsoft, a link for an
// ICS feed), how its address is found, and the adapter that reads it.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/oauth2"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/google"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/icsfeed"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/microsoft"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/oauth"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

type Kinds struct {
	Store     *store.Store
	Google    *oauth.Provider // nil: not set up (no client id)
	Microsoft *oauth.Provider
}

// Provider is the sign-in of a kind, nil when it isn't set up or the kind isn't signed into.
func (k *Kinds) Provider(kind string) *oauth.Provider {
	switch kind {
	case "google":
		return k.Google
	case "microsoft":
		return k.Microsoft
	}
	return nil
}

// FeedConfig is an ICS feed account's config.
type FeedConfig struct {
	URL string `json:"url"`
}

// Adapter is the engine's way to an account's source.
func (k *Kinds) Adapter(a store.Account) (adapter.Adapter, error) {
	switch a.Kind {
	case "ics":
		var c FeedConfig
		if err := json.Unmarshal([]byte(a.Config), &c); err != nil {
			return nil, fmt.Errorf("account %d: bad config: %w", a.ID, err)
		}
		return &icsfeed.Feed{URL: c.URL}, nil
	case "google", "microsoft":
		p := k.Provider(a.Kind)
		if p == nil {
			return nil, fmt.Errorf("account %d: %s sign-in isn't set up (%s_CLIENT_ID)", a.ID, a.Kind, strings.ToUpper(a.Kind))
		}
		s, err := oauth.Load(a.Config)
		if err != nil {
			return nil, fmt.Errorf("account %d: %w", a.ID, err)
		}
		save := func(tok *oauth2.Token) error {
			s.Token = tok
			return k.Store.SetAccountConfig(context.Background(), a.ID, s.Encode())
		}
		hc := p.Client(context.Background(), s.Token, save)
		if a.Kind == "google" {
			return signedIn{&google.Calendar{HTTP: hc, Address: a.Address}}, nil
		}
		return signedIn{&microsoft.Calendar{HTTP: hc, Address: a.Address}}, nil
	}
	return nil, fmt.Errorf("account %d: unknown kind %q", a.ID, a.Kind)
}

// ErrSignIn: the account's sign-in is over (revoked, expired, password changed).
var ErrSignIn = errors.New("the account must be signed into again on the accounts page")

// signedIn says plainly when an account's sign-in is over: on the accounts page, and on an answer it couldn't send.
type signedIn struct{ adapter.Adapter }

func (s signedIn) List(ctx context.Context, token string) (adapter.Page, error) {
	page, err := s.Adapter.List(ctx, token)
	if oauth.SignInAgain(err) {
		return page, fmt.Errorf("%w: %v", ErrSignIn, err)
	}
	return page, err
}

func (s signedIn) RSVP(ctx context.Context, sourceID, partstat string) error {
	err := s.Adapter.RSVP(ctx, sourceID, partstat)
	if oauth.SignInAgain(err) {
		return &adapter.Refused{Reason: "la connexion à ce compte a expiré : reconnecte-le sur la page des comptes",
			Err: fmt.Errorf("%w: %v", ErrSignIn, err)}
	}
	return err
}

// Address is the address of the account just signed into: the one invitations reach.
func (k *Kinds) Address(ctx context.Context, kind string, tok *oauth2.Token) (string, error) {
	p := k.Provider(kind)
	if p == nil {
		return "", fmt.Errorf("%s sign-in isn't set up", kind)
	}
	claims, _ := oauth.IDClaims(tok)
	switch kind {
	case "google":
		if claims.Email == "" || claims.EmailVerified != nil && !*claims.EmailVerified {
			return "", errors.New("google: no verified address in the sign-in")
		}
		return claims.Email, nil
	case "microsoft":
		// The mailbox's address: a school sign-in name can differ from the address mail is sent to.
		cal := &microsoft.Calendar{HTTP: p.Client(ctx, tok, func(*oauth2.Token) error { return nil })}
		addr, err := cal.Owner(ctx)
		if err == nil {
			return addr, nil
		}
		if claims.Email != "" {
			return claims.Email, nil
		}
		if strings.Contains(claims.PreferredUsername, "@") {
			return claims.PreferredUsername, nil
		}
		return "", err
	}
	return "", fmt.Errorf("unknown kind %q", kind)
}
