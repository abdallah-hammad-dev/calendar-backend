package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/oauth"
)

// The OAuth clients behind Google and Microsoft sign-ins (docs/comptes.md) come from the server's environment
// (GOOGLE_CLIENT_ID…, deploy/.env), which wins, or else from the accounts page, kept in the settings table.

// OAuthKinds are the kinds signed into through an OAuth client of mine, in the page's order.
var OAuthKinds = []string{"google", "microsoft"}

// ProviderSettings is the OAuth client a kind signs in with.
type ProviderSettings struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Tenant       string `json:"tenant,omitempty"` // Microsoft: "common", "organizations", "consumers", an ID or a domain
}

func (p ProviderSettings) complete() bool { return p.ClientID != "" && p.ClientSecret != "" }

// NewProvider is kind's sign-in with these settings, sending the browser back to callback.
func NewProvider(kind string, p ProviderSettings, callback string) *oauth.Provider {
	if kind == "google" {
		return oauth.Google(p.ClientID, p.ClientSecret, callback)
	}
	tenant := p.Tenant
	if tenant == "" {
		tenant = "common"
	}
	return oauth.Microsoft(p.ClientID, p.ClientSecret, tenant, callback)
}

func settingKey(kind string) string { return "oauth_client_" + kind }

// Saved is what was entered on the page for kind (empty if nothing was).
func (k *Kinds) Saved(ctx context.Context, kind string) (ProviderSettings, error) {
	var p ProviderSettings
	v, err := k.Store.Setting(ctx, settingKey(kind))
	if err != nil || v == "" {
		return p, err
	}
	return p, json.Unmarshal([]byte(v), &p)
}

// Save keeps settings entered on the page (one row: never half of them) and puts the sign-in in place at once.
func (k *Kinds) Save(ctx context.Context, kind string, p ProviderSettings, callback string) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := k.Store.SetSetting(ctx, settingKey(kind), string(b)); err != nil {
		return err
	}
	k.SetProvider(kind, NewProvider(kind, p, callback))
	return nil
}

// Setup puts each kind's sign-in in place at startup: from env when its client ID is there, else from the page's
// settings. It says which kinds env set (the page shows those and doesn't change them).
func (k *Kinds) Setup(ctx context.Context, env map[string]ProviderSettings, callback string) (map[string]bool, error) {
	fromEnv := map[string]bool{}
	for _, kind := range OAuthKinds {
		p := env[kind]
		if p.ClientID != "" {
			fromEnv[kind] = true
		} else {
			var err error
			if p, err = k.Saved(ctx, kind); err != nil {
				return nil, err
			}
		}
		if p.complete() {
			k.SetProvider(kind, NewProvider(kind, p, callback))
		} else {
			k.SetProvider(kind, nil)
		}
	}
	return fromEnv, nil
}

var (
	googleClientID = regexp.MustCompile(`^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$`)
	guid           = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tenantName     = regexp.MustCompile(`^(common|organizations|consumers|[a-z0-9-]+(\.[a-z0-9-]+)+)$`)
)

// Check says what is wrong with settings typed on the page for kind, in French, as the page shows it.
func (p ProviderSettings) Check(kind string) error {
	switch kind {
	case "google":
		if !googleClientID.MatchString(p.ClientID) {
			return errors.New("L'ID client Google ressemble à 1234567890-abc123.apps.googleusercontent.com (console Google Cloud, Clients).")
		}
	case "microsoft":
		if !guid.MatchString(p.ClientID) {
			return errors.New("L'ID d'application Microsoft est de la forme 0a1b2c3d-…-…, sur la page « Vue d'ensemble » de l'app.")
		}
		if !guid.MatchString(p.Tenant) && !tenantName.MatchString(p.Tenant) {
			return errors.New("Annuaire : common, organizations, consumers, l'ID de l'annuaire ou son domaine.")
		}
		if guid.MatchString(p.ClientSecret) {
			return errors.New("C'est l'ID du secret : copie plutôt sa Valeur (Certificats et secrets), montrée une seule fois à sa création.")
		}
	default:
		return errors.New("connexion inconnue : " + kind)
	}
	if p.ClientSecret == "" {
		return errors.New("Donne le secret du client.")
	}
	if len(p.ClientSecret) > 512 || strings.IndexFunc(p.ClientSecret, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return errors.New("Le secret ne contient ni espace ni retour à la ligne : recopie-le.")
	}
	return nil
}
