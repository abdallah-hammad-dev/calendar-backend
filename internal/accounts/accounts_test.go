package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/icsfeed"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/oauth"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

type failing struct{ err error }

func (f failing) List(context.Context, string) (adapter.Page, error) { return adapter.Page{}, f.err }
func (f failing) RSVP(context.Context, string, string) error         { return f.err }

func TestSignInOver(t *testing.T) {
	over := fmt.Errorf("Get …: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant"})
	ad := signedIn{failing{over}}
	if _, err := ad.List(context.Background(), ""); !errors.Is(err, ErrSignIn) {
		t.Fatalf("listing: %v", err)
	}
	var r *adapter.Refused
	if err := ad.RSVP(context.Background(), "id", "ACCEPTED"); !errors.As(err, &r) || !strings.Contains(r.Reason, "reconnecte") {
		t.Fatalf("answer: %v", err)
	}
	// Anything else is left as it is (tried again).
	down := errors.New("network down")
	if err := (signedIn{failing{down}}).RSVP(context.Background(), "id", "ACCEPTED"); err != down {
		t.Fatal(err)
	}
}

func TestAdapter(t *testing.T) {
	k := &Kinds{}
	if ad, err := k.Adapter(store.Account{Kind: "ics", Config: `{"url":"https://x.test/a.ics"}`}); err != nil || ad.(*icsfeed.Feed).URL != "https://x.test/a.ics" {
		t.Fatalf("%v %v", ad, err)
	}
	if _, err := k.Adapter(store.Account{Kind: "google", Config: `{}`}); err == nil || !strings.Contains(err.Error(), "GOOGLE_CLIENT_ID") {
		t.Fatalf("Google not set up: %v", err)
	}
	k.Google = oauth.Google("id", "secret", "https://x/cb")
	if _, err := k.Adapter(store.Account{Kind: "google", Config: `{}`}); err == nil {
		t.Fatal("an account without its sign-in")
	}
	if _, err := k.Adapter(store.Account{Kind: "google", Config: oauth.Stored{Token: &oauth2.Token{RefreshToken: "r"}}.Encode()}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Adapter(store.Account{Kind: "caldav"}); err == nil {
		t.Fatal("unknown kind")
	}
}
