package web

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/accounts"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/mirror"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/oauth"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

// fakeGoogle is a token endpoint signing into address.
func fakeGoogle(t *testing.T, address *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + *address + `","email_verified":true}`))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":3600,"id_token":"h.%s.s"}`, claims)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type env struct {
	srv    *httptest.Server
	client *http.Client
	store  *store.Store
	server *Server
}

func setup(t *testing.T, dc *davis.Client, home, me string, google *oauth.Provider) env {
	st, err := store.Open(filepath.Join(t.TempDir(), "backend.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	kinds := &accounts.Kinds{Store: st, Google: google}
	paris, _ := time.LoadLocation("Europe/Paris")
	e := &mirror.Engine{Store: st, Davis: dc, Me: me, Home: home, Loc: paris, Adapter: kinds.Adapter, Log: log}
	s := &Server{Engine: e, Store: st, Kinds: kinds, BaseURL: "http://cal.test", User: "abd", Password: "secret", Log: log}
	h, err := s.Handler(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return env{srv: srv, client: client, store: st, server: s}
}

func (e env) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := e.client.PostForm(e.srv.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e env) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := e.client.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e env) login(t *testing.T) {
	t.Helper()
	if resp := e.post(t, "/login", url.Values{"user": {"abd"}, "password": {"secret"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %s", resp.Status)
	}
}

// Davis unreachable: the page still opens, and says so.
func offline(t *testing.T) *davis.Client {
	dc, _ := davis.New("http://127.0.0.1:1/dav/", "abd", "x", nil)
	return dc
}

func TestLoginAndSession(t *testing.T) {
	e := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", nil)
	if resp, _ := e.get(t, "/healthz"); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if resp, _ := e.get(t, "/"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("not logged in: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	if resp, body := e.get(t, "/privacy"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "utilisation limitée") {
		t.Fatalf("privacy policy without logging in: %s", resp.Status)
	}
	if _, body := e.get(t, "/login"); !strings.Contains(body, `href="/privacy"`) {
		t.Fatal("the login page has no link to the privacy policy")
	}
	if resp := e.post(t, "/sync", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an action without logging in: %s", resp.Status)
	}
	start := time.Now()
	if resp := e.post(t, "/login", url.Values{"user": {"abd"}, "password": {"nope"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %s", resp.Status)
	}
	if time.Since(start) < time.Second {
		t.Fatal("a failed login answered at once")
	}

	e.login(t)
	resp, body := e.get(t, "/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Davis ne répond pas") || !strings.Contains(body, "http://cal.test/oauth/callback") {
		t.Fatalf("home: %s\n%s", resp.Status, body)
	}
	if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("headers missing")
	}

	// A forged session isn't one.
	u, _ := url.Parse(e.srv.URL)
	exp := time.Now().Add(time.Hour).Unix()
	e.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: fmt.Sprintf("%d.AAAA", exp)}})
	if resp, _ := e.get(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("forged session accepted")
	}
	e.login(t)

	// Another site can't post here with my session.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/sync", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	cross, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cross.Body.Close()
	if cross.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site post: %s", cross.Status)
	}
	if resp := e.post(t, "/sync", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sync: %s", resp.Status)
	}

	// A callback that doesn't come from a sign-in started here.
	if resp, body := e.get(t, CallbackPath+"?state=made-up&code=x"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "expiré") {
		t.Fatalf("unknown state: %s", resp.Status)
	}

	if resp := e.post(t, "/logout", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatal(resp.Status)
	}
	if resp, _ := e.get(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("still logged in after logging out")
	}
}

func TestFeedForm(t *testing.T) {
	e := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", nil)
	e.login(t)
	for _, f := range []url.Values{
		{"name": {"x"}, "address": {"me@school.fr"}, "url": {"file:///etc/passwd"}},
		{"name": {"x"}, "address": {"not an address"}, "url": {"https://x.test/a.ics"}},
		{"name": {""}, "address": {"me@school.fr"}, "url": {"https://x.test/a.ics"}},
	} {
		if resp := e.post(t, "/accounts/ics", f); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: %s", f, resp.Status)
		}
	}
}

// The whole sign-in against the bench Davis (DAVIS_TEST_URL), as orga: an account added through a (fake) Google,
// signed in again, refused with another address, removed.
func TestSignInAgainstDavis(t *testing.T) {
	davisURL := os.Getenv("DAVIS_TEST_URL")
	if davisURL == "" {
		t.Skip("DAVIS_TEST_URL not set")
	}
	ctx := context.Background()
	user := os.Getenv("DAVIS_TEST_USER")
	if user == "" {
		user = "orga"
	}
	pass := os.Getenv("DAVIS_TEST_PASSWORD")
	if pass == "" {
		pass = "x"
	}
	dc, err := davis.New(davisURL, user, pass, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := dc.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	coll := p.Home + "it-web-" + suffix + "/"
	if err := dc.MakeCalendar(ctx, coll, "Web test "+suffix, "", "VEVENT"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Delete(context.Background(), coll, "") })

	address := "me@gmail.com"
	tokens := fakeGoogle(t, &address)
	g := oauth.Google("id", "secret", "http://cal.test"+CallbackPath)
	g.Config.Endpoint = oauth2.Endpoint{AuthURL: "https://accounts.test/auth", TokenURL: tokens.URL, AuthStyle: oauth2.AuthStyleInParams}
	e := setup(t, dc, p.Home, p.Email(), g)
	e.login(t)
	if _, body := e.get(t, "/"); !strings.Contains(body, "Web test "+suffix) {
		t.Fatal("my collection isn't offered")
	}

	signIn := func(form url.Values) (*http.Response, string) {
		t.Helper()
		resp := e.post(t, "/accounts/google/connect", form)
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusSeeOther || loc.Host != "accounts.test" || loc.Query().Get("code_challenge") == "" {
			t.Fatalf("not sent to Google: %s %s", resp.Status, loc)
		}
		return e.get(t, CallbackPath+"?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")))
	}

	resp, _ := signIn(url.Values{"name": {"Gmail"}, "collection": {coll}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?ok=added" {
		t.Fatalf("callback: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	list, _ := e.store.Accounts(ctx)
	if len(list) != 1 || list[0].Address != "me@gmail.com" || list[0].Kind != "google" || !davis.SamePath(list[0].Collection, coll) {
		t.Fatalf("accounts: %+v", list)
	}
	if s, err := oauth.Load(list[0].Config); err != nil || s.Token.RefreshToken != "r" {
		t.Fatalf("token not kept: %v", err)
	}
	if _, body := e.get(t, "/"); !strings.Contains(body, "Gmail") || !strings.Contains(body, "Reconnecter") {
		t.Fatal("account not listed")
	}

	// The same address again: refused.
	if resp, body := signIn(url.Values{"name": {"Gmail 2"}, "collection": {coll}}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "déjà ajouté") {
		t.Fatalf("duplicate: %s", resp.Status)
	}
	// Signed in again: the same address only.
	id := fmt.Sprint(list[0].ID)
	if resp, _ := signIn(url.Values{"account": {id}}); resp.Header.Get("Location") != "/?ok=reconnected" {
		t.Fatalf("reconnect: %s", resp.Status)
	}
	address = "other@gmail.com"
	if resp, body := signIn(url.Values{"account": {id}}); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "other@gmail.com") {
		t.Fatalf("reconnect with another address: %s", resp.Status)
	}
	// A state is good once.
	resp = e.post(t, "/accounts/google/connect", url.Values{"account": {id}})
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := url.QueryEscape(loc.Query().Get("state"))
	e.get(t, CallbackPath+"?error=access_denied&state="+state)
	if resp, _ := e.get(t, CallbackPath+"?code=the-code&state="+state); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("a state used twice")
	}

	if resp := e.post(t, "/accounts/"+id+"/remove", url.Values{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("removed without saying what becomes of its events")
	}
	if resp := e.post(t, "/accounts/"+id+"/remove", url.Values{"events": {"delete"}}); resp.Header.Get("Location") != "/?ok=removed" {
		t.Fatalf("remove: %s", resp.Status)
	}
	if list, _ := e.store.Accounts(ctx); len(list) != 0 {
		t.Fatal("still there")
	}
}
