package accounts

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

const googleID = "123456789012-abcdef0123456789.apps.googleusercontent.com"

func kindsWithStore(t *testing.T) *Kinds {
	st, err := store.Open(filepath.Join(t.TempDir(), "backend.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Kinds{Store: st}
}

// The environment wins; a kind it leaves empty takes what the page saved; nothing saved, no sign-in.
func TestSetup(t *testing.T) {
	ctx := context.Background()
	k := kindsWithStore(t)
	if err := k.Save(ctx, "google", ProviderSettings{ClientID: "page-id", ClientSecret: "page-secret"}, "cb"); err != nil {
		t.Fatal(err)
	}
	if err := k.Save(ctx, "microsoft", ProviderSettings{ClientID: "ms-page", ClientSecret: "s", Tenant: "school.example"}, "cb"); err != nil {
		t.Fatal(err)
	}

	fromEnv, err := k.Setup(ctx, map[string]ProviderSettings{
		"google":    {ClientID: "env-id", ClientSecret: "env-secret"},
		"microsoft": {Tenant: "common"}, // MICROSOFT_TENANT alone, as deploy.sh writes it: not a client
	}, "https://accounts.test/oauth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if !fromEnv["google"] || fromEnv["microsoft"] {
		t.Fatalf("from env: %v", fromEnv)
	}
	if g := k.Provider("google"); g == nil || g.Config.ClientID != "env-id" || g.Config.ClientSecret != "env-secret" ||
		g.Config.RedirectURL != "https://accounts.test/oauth/callback" {
		t.Fatalf("google: %+v", g)
	}
	if m := k.Provider("microsoft"); m == nil || m.Config.ClientID != "ms-page" || m.Tenant != "school.example" ||
		!strings.Contains(m.Config.Endpoint.AuthURL, "/school.example/") {
		t.Fatalf("microsoft: %+v", m)
	}

	empty := kindsWithStore(t)
	if _, err := empty.Setup(ctx, nil, "cb"); err != nil || empty.Provider("google") != nil || empty.Provider("microsoft") != nil {
		t.Fatalf("nothing set up: %v", err)
	}
}

func TestCheck(t *testing.T) {
	const appID = "0f1e2d3c-4b5a-4697-8a1b-2c3d4e5f6a7b"
	for _, c := range []struct {
		kind string
		p    ProviderSettings
		bad  string // a word of the message, "" = accepted
	}{
		{"google", ProviderSettings{ClientID: googleID, ClientSecret: "GOCSPX-abc_DEF-123"}, ""},
		{"google", ProviderSettings{ClientID: "my-project", ClientSecret: "x"}, "googleusercontent"},
		{"google", ProviderSettings{ClientID: googleID}, "secret"},
		{"google", ProviderSettings{ClientID: googleID, ClientSecret: "two words"}, "espace"},
		{"microsoft", ProviderSettings{ClientID: appID, ClientSecret: "abc8Q~xyz.-_", Tenant: "common"}, ""},
		{"microsoft", ProviderSettings{ClientID: appID, ClientSecret: "abc8Q~xyz", Tenant: "7a8b9c0d-1e2f-4a3b-9c4d-5e6f7a8b9c0d"}, ""},
		{"microsoft", ProviderSettings{ClientID: appID, ClientSecret: "abc8Q~xyz", Tenant: "school.example"}, ""},
		{"microsoft", ProviderSettings{ClientID: "calendar-backend", ClientSecret: "s", Tenant: "common"}, "Vue d'ensemble"},
		{"microsoft", ProviderSettings{ClientID: appID, ClientSecret: "s", Tenant: "my school"}, "Annuaire"},
		// The secret's ID, copied instead of its value.
		{"microsoft", ProviderSettings{ClientID: appID, ClientSecret: "3c2b1a09-8f7e-4d6c-b5a4-938271605f4e", Tenant: "common"}, "Valeur"},
		{"ics", ProviderSettings{ClientID: "x", ClientSecret: "y"}, "inconnue"},
	} {
		err := c.p.Check(c.kind)
		switch {
		case c.bad == "" && err != nil:
			t.Errorf("%s %+v refused: %v", c.kind, c.p, err)
		case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)):
			t.Errorf("%s %+v: want %q in the error, got %v", c.kind, c.p, c.bad, err)
		}
	}
}
