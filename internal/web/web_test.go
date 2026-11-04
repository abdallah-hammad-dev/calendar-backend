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

func setup(t *testing.T, dc *davis.Client, home, me string, google *oauth.Provider, opts ...func(*Server)) env {
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
	for _, o := range opts {
		o(s)
	}
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

// Sign-ins set on the page: in place at once, shown without their secret; a blank secret keeps the one kept; a new
// client says to sign the kind's accounts in again; a kind the environment sets is shown, not changed.
func TestProviderSettings(t *testing.T) {
	ctx := context.Background()
	e := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", nil)
	e.login(t)
	if _, body := e.get(t, "/"); !strings.Contains(body, `action="/settings/google"`) ||
		!strings.Contains(body, `action="/settings/microsoft"`) || !strings.Contains(body, "Non configurée") {
		t.Fatal("no settings forms")
	}
	settle := func(kind string, form url.Values, want string) {
		t.Helper()
		resp := e.post(t, "/settings/"+kind, form)
		if got := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || got != want {
			t.Fatalf("%s %v: %s %s, want %s", kind, form, resp.Status, got, want)
		}
	}

	const id = "123456789012-abcdef0123456789.apps.googleusercontent.com"
	if resp := e.post(t, "/settings/google", url.Values{"client_id": {"my-project"}, "client_secret": {"s"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad client id: %s", resp.Status)
	}
	if e.server.Kinds.Provider("google") != nil {
		t.Fatal("set from a refused form")
	}
	settle("google", url.Values{"client_id": {id}, "client_secret": {"GOCSPX-first"}}, "/?ok=google-set")
	if g := e.server.Kinds.Provider("google"); g == nil || g.Config.ClientID != id || g.Config.ClientSecret != "GOCSPX-first" ||
		g.Config.RedirectURL != "http://cal.test"+CallbackPath {
		t.Fatalf("google: %+v", g)
	}
	_, body := e.get(t, "/")
	if strings.Contains(body, "GOCSPX-first") {
		t.Fatal("the secret is on the page")
	}
	if !strings.Contains(body, id) || !strings.Contains(body, "Enregistré (vide = inchangé)") {
		t.Fatal("the client isn't shown")
	}

	// An account signed in through this client: another client means signing it in again.
	if _, err := e.store.AddAccount(ctx, store.Account{Kind: "google", Name: "Gmail", Address: "me@gmail.com",
		Collection: "/dav/calendars/abd/c/", Config: "{}"}); err != nil {
		t.Fatal(err)
	}
	const other = "999999999999-zzzz.apps.googleusercontent.com"
	settle("google", url.Values{"client_id": {other}, "client_secret": {""}}, "/?ok=google-set-reconnect")
	if g := e.server.Kinds.Provider("google"); g.Config.ClientID != other || g.Config.ClientSecret != "GOCSPX-first" {
		t.Fatalf("blank secret: %+v", g.Config)
	}
	settle("google", url.Values{"client_id": {other}, "client_secret": {"GOCSPX-second"}}, "/?ok=google-set")
	if g := e.server.Kinds.Provider("google"); g.Config.ClientSecret != "GOCSPX-second" {
		t.Fatal("new secret not taken")
	}

	// Microsoft: its secret's ID instead of its value is refused; the directory is kept as typed, in lower case.
	const app = "11111111-2222-4333-8444-555555555555"
	if resp := e.post(t, "/settings/microsoft", url.Values{"client_id": {app},
		"client_secret": {"66666666-7777-4888-9999-aaaaaaaaaaaa"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("secret ID taken as the secret: %s", resp.Status)
	}
	settle("microsoft", url.Values{"client_id": {app}, "client_secret": {"abc8Q~value"}, "tenant": {"School.example"}}, "/?ok=microsoft-set")
	if m := e.server.Kinds.Provider("microsoft"); m == nil || m.Tenant != "school.example" || !strings.Contains(m.Config.Endpoint.AuthURL, "/school.example/") {
		t.Fatalf("microsoft: %+v", m)
	}
	if saved, _ := e.server.Kinds.Saved(ctx, "microsoft"); saved.ClientSecret != "abc8Q~value" {
		t.Fatal("not kept")
	}

	// Set by the environment: shown, read-only.
	g := oauth.Google(id, "env-secret", "http://cal.test"+CallbackPath)
	envd := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", g,
		func(s *Server) { s.FromEnv = map[string]bool{"google": true} })
	envd.login(t)
	if resp := envd.post(t, "/settings/google", url.Values{"client_id": {other}, "client_secret": {"x"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("changed what the environment sets: %s", resp.Status)
	}
	_, body = envd.get(t, "/")
	if strings.Contains(body, `action="/settings/google"`) || !strings.Contains(body, "deploy/.env") || !strings.Contains(body, id) ||
		strings.Contains(body, "env-secret") || !strings.Contains(body, `action="/settings/microsoft"`) {
		t.Fatal("environment's sign-in not shown as such")
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

	resp, _ := signIn(url.Values{"name": {""}, "collection": {coll}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?ok=added" {
		t.Fatalf("callback: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	list, _ := e.store.Accounts(ctx)
	if len(list) != 1 || list[0].Address != "me@gmail.com" || list[0].Name != "me@gmail.com" || list[0].Kind != "google" ||
		!davis.SamePath(list[0].Collection, coll) {
		t.Fatalf("accounts: %+v", list)
	}
	if s, err := oauth.Load(list[0].Config); err != nil || s.Token.RefreshToken != "r" {
		t.Fatalf("token not kept: %v", err)
	}
	if _, body := e.get(t, "/"); !strings.Contains(body, "me@gmail.com") || !strings.Contains(body, "Reconnecter") {
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

// The trash on the page: an event and a date of a series I deleted, the last first; restoring what is gone says so.
func TestTrash(t *testing.T) {
	ctx := context.Background()
	e := setup(t, offline(t), "/dav/calendars/abd/", "abd@bench.test", nil)
	e.login(t)
	if _, body := e.get(t, "/"); !strings.Contains(body, "Corbeille") || !strings.Contains(body, "Vide.") {
		t.Fatal("no empty trash on the page")
	}
	id, err := e.store.AddAccount(ctx, store.Account{Kind: "ics", Name: "École", Address: "me@school.fr", Config: "{}",
		Collection: "/dav/calendars/abd/ecole/"})
	if err != nil {
		t.Fatal(err)
	}
	src := func(uid, summary, start, rule string) string {
		return strings.ReplaceAll("BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:x\nBEGIN:VEVENT\nUID:"+uid+"\nDTSTAMP:20261020T080000Z\nSUMMARY:"+
			summary+"\nDTSTART:"+start+"\n"+rule+"END:VEVENT\nEND:VCALENDAR\n", "\n", "\r\n")
	}
	deleted := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	for _, it := range []store.Item{
		{AccountID: id, UID: "one", SourceData: src("one", "Soutenance\\, salle 12", "20261123T090000Z", ""), Trashed: deleted},
		{AccountID: id, UID: "weekly", SourceData: src("weekly", "Cours réseau", "20261102T130000Z", "RRULE:FREQ=WEEKLY\n"), Href: "/dav/calendars/abd/ecole/weekly.ics",
			Removed: []store.Removed{{Line: "EXDATE;TZID=Europe/Paris:20261110T140000", At: deleted.Add(time.Hour)}}},
	} {
		if err := e.store.PutItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	_, body := e.get(t, "/")
	date, event := strings.Index(body, "mar. 10/11/2026 14:00 · une date de la série · École"), strings.Index(body, "lun. 23/11/2026 10:00 · École · supprimé le 04/11/2026")
	if date < 0 || event < 0 || date > event || !strings.Contains(body, "Soutenance, salle 12") ||
		!strings.Contains(body, `name="date" value="EXDATE;TZID=Europe/Paris:20261110T140000"`) {
		t.Fatalf("trash rows:\n%s", body)
	}
	resp := e.post(t, "/trash/restore", url.Values{"account": {fmt.Sprint(id)}, "uid": {"gone"}})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "plus là") {
		t.Fatalf("restoring what is gone: %s\n%s", resp.Status, b)
	}
}
