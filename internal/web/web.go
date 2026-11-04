// Package web is the accounts page (spec §7): sign in with the Davis user, add a Google, Microsoft or ICS feed
// account and the collection its events go into, see how each account's last pass went, sign one in again, remove
// one. It is one person's page (one backend per person): one login, the Davis user's.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	"golang.org/x/oauth2"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/accounts"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/ics"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/mirror"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/oauth"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

//go:embed templates/*.html
var templates embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"kind": kindLabel,
	// card: a Google or Microsoft "add" card, its sign-in and the collections offered.
	"card": func(p provider, c []davis.Calendar) addCard { return addCard{P: p, Collections: c} },
	// collections: the select of collections an account writes into, with its element ID.
	"collections": func(id string, c []davis.Calendar) collectionSelect { return collectionSelect{ID: id, List: c} },
}).ParseFS(templates, "templates/*.html"))

type addCard struct {
	P           provider
	Collections []davis.Calendar
}

type collectionSelect struct {
	ID   string
	List []davis.Calendar
}

// CallbackPath is where providers send the browser back after sign-in: BASE_URL + CallbackPath is the redirect URI
// set up at Google and Microsoft.
const CallbackPath = "/oauth/callback"

const (
	sessionCookie = "session"
	sessionLife   = 30 * 24 * time.Hour
	signInLife    = 15 * time.Minute
)

type Server struct {
	Engine   *mirror.Engine
	Store    *store.Store
	Kinds    *accounts.Kinds
	FromEnv  map[string]bool // kinds whose OAuth client the environment sets: shown, not changed, here
	BaseURL  string          // the page's public address, no trailing slash
	User     string          // the Davis user and password: the page's login
	Password string
	Log      *slog.Logger
	// MCP is the Claude connector's endpoint (spec §10.4), served at MCPPath to the clients signed in here.
	MCP http.Handler

	key     []byte
	loginMu sync.Mutex // failed logins wait one at a time

	mu      sync.Mutex
	signIns map[string]signIn    // by OAuth state
	codes   map[string]authCode  // the connector's authorization codes, by hash
	touched map[string]time.Time // when a connector client's last use was saved
}

// signIn is a sign-in under way at a provider.
type signIn struct {
	Kind       string
	Name       string
	Collection string // href
	AccountID  int64  // signing an existing account in again
	Verifier   string
	Started    time.Time
}

// Handler is the page, with its session key (made once, kept in the database).
func (s *Server) Handler(ctx context.Context) (http.Handler, error) {
	key, err := s.Store.Setting(ctx, "session_key")
	if err != nil {
		return nil, err
	}
	if key == "" {
		b := make([]byte, 32)
		rand.Read(b)
		key = hex.EncodeToString(b)
		if err := s.Store.SetSetting(ctx, "session_key", key); err != nil {
			return nil, err
		}
	}
	s.key, _ = hex.DecodeString(key)
	s.signIns = map[string]signIn{}
	s.codes = map[string]authCode{}
	s.touched = map[string]time.Time{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	// Public: Google wants a privacy policy's address before an app leaves "Testing" (docs/comptes.md).
	mux.HandleFunc("GET /privacy", func(w http.ResponseWriter, r *http.Request) { s.render(w, http.StatusOK, "privacy", page{}) })
	mux.Handle("GET /{$}", s.auth(s.home))
	mux.Handle("POST /accounts/ics", s.auth(s.addFeed))
	mux.Handle("POST /accounts/{kind}/connect", s.auth(s.connect))
	mux.Handle("GET "+CallbackPath, s.auth(s.callback))
	mux.Handle("POST /accounts/{id}/remove", s.auth(s.remove))
	mux.Handle("POST /trash/restore", s.auth(s.restore))
	mux.Handle("POST /settings/{kind}", s.auth(s.saveProvider))
	mux.Handle("POST /sync", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.Engine.Wake()
		http.Redirect(w, r, "/?ok=sync", http.StatusSeeOther)
	}))
	cop := http.NewCrossOriginProtection()
	s.oauthRoutes(mux, cop)
	return headers(cop.Handler(mux)), nil
}

// headers: never framed, cached, nor sending the address (an OAuth code) to another site.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// --- session

func (s *Server) sign(expires int64) string {
	m := hmac.New(sha256.New, s.key)
	fmt.Fprint(m, expires)
	return strconv.FormatInt(expires, 10) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, _, ok := strings.Cut(c.Value, ".")
	n, err := strconv.ParseInt(exp, 10, 64)
	return ok && err == nil && time.Now().Unix() < n && hmac.Equal([]byte(s.sign(n)), []byte(c.Value))
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			s.render(w, http.StatusUnauthorized, "error", page{Error: "Ta session a expiré : reconnecte-toi.", Back: "/login"})
			return
		}
		h(w, r)
	})
}

func (s *Server) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: strings.HasPrefix(s.BaseURL, "https://"), SameSite: http.SameSiteLaxMode}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := afterLogin(r.URL.Query().Get("next"))
	if s.loggedIn(r) {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", page{Next: next})
}

// afterLogin is where the login goes on to: the connector's consent page it was sent from, or the accounts page
// (never another address: the link could be anyone's).
func afterLogin(next string) string {
	if strings.HasPrefix(next, "/authorize?") {
		return next
	}
	return "/"
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	user, pass := r.PostFormValue("user"), r.PostFormValue("password")
	okUser := subtle.ConstantTimeCompare([]byte(user), []byte(s.User)) == 1
	okPass := subtle.ConstantTimeCompare([]byte(pass), []byte(s.Password)) == 1
	if !okUser || !okPass {
		// Slow, and one at a time: guessing the password takes a second per try.
		s.loginMu.Lock()
		time.Sleep(time.Second)
		s.loginMu.Unlock()
		s.Log.Warn("failed login on the accounts page", "from", r.RemoteAddr)
		s.render(w, http.StatusUnauthorized, "login", page{Error: "Identifiant ou mot de passe incorrect.",
			Next: afterLogin(r.PostFormValue("next"))})
		return
	}
	exp := time.Now().Add(sessionLife).Unix()
	http.SetCookie(w, s.cookie(s.sign(exp), int(sessionLife.Seconds())))
	http.Redirect(w, r, afterLogin(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.cookie("", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- the page

type page struct {
	Error, Notice string
	Back          string // the error page's link back
	Next          string // the login's next page
	Me            string // my address on Davis
	Accounts      []row
	Trash         []trashRow
	Claude        []claudeRow // the connector's clients I signed in
	ConnectorURL  string
	Collections   []davis.Calendar
	Google        provider
	Microsoft     provider
	CallbackURL   string
}

// provider is a kind's sign-in as the page shows it: set or not, from the environment or from here. Never its
// secret, only whether one is kept.
type provider struct {
	Kind, Label string
	EnvPrefix   string // GOOGLE, MICROSOFT
	On          bool   // accounts of this kind can be added
	FromEnv     bool
	ClientID    string
	Tenant      string
	HasSecret   bool
	Accounts    int // accounts of this kind: changing the client means signing them in again
	BaseURL     string
	CallbackURL string
}

type row struct {
	store.Account
	CollectionName string
	LastSyncText   string
	SignIn         bool // signed into at a provider: can be signed in again
	SignInOver     bool
}

// claudeRow is a client of the connector, as the page shows it.
type claudeRow struct {
	ID, Name     string
	Signed       bool
	LastUsedText string
}

// trashRow is one thing in the trash (spec §5.4): an account's event I deleted, or one date of its series (Line).
type trashRow struct {
	AccountID int64
	UID       string
	Line      string // the date's EXDATE line; "" for the whole event
	Title     string
	When      string
	Account   string
	Deleted   string
	at        time.Time
}

var notices = map[string]string{
	"added":          "Compte ajouté : ses événements arrivent dans la collection choisie (la première fois peut prendre quelques minutes).",
	"reconnected":    "Compte reconnecté.",
	"removed":        "Compte retiré.",
	"restored":       "Restauré : il revient dans ton calendrier, à jour.",
	"sync":           "Synchronisation lancée.",
	"claude-revoked": "Connecteur déconnecté : pour t'en resservir, retire-le de Claude et ajoute-le à nouveau.",
	"google-set":     "Connexion Google enregistrée : tu peux ajouter un compte Google.",
	"google-set-reconnect": "Connexion Google enregistrée. Tes comptes Google ont été connectés avec l'ancien réglage : " +
		"reconnecte-les (bouton « Reconnecter »).",
	"microsoft-set": "Connexion Microsoft enregistrée : tu peux ajouter un compte Microsoft.",
	"microsoft-set-reconnect": "Connexion Microsoft enregistrée. Tes comptes Microsoft ont été connectés avec l'ancien " +
		"réglage : reconnecte-les (bouton « Reconnecter »).",
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := page{Notice: notices[r.URL.Query().Get("ok")], Me: s.Engine.Me, CallbackURL: s.BaseURL + CallbackPath}
	cols, err := s.collections(ctx)
	if err != nil {
		p.Error = "Davis ne répond pas : " + err.Error()
	}
	p.Collections = cols
	names := map[string]string{}
	for _, c := range cols {
		names[c.Href] = c.DisplayName
	}
	list, err := s.Store.Accounts(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	if p.Google, err = s.provider(ctx, "google", list); err == nil {
		p.Microsoft, err = s.provider(ctx, "microsoft", list)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, a := range list {
		rw := row{Account: a, CollectionName: names[a.Collection], SignIn: s.Kinds.Provider(a.Kind) != nil,
			SignInOver: strings.Contains(a.LastError, accounts.ErrSignIn.Error())}
		if rw.CollectionName == "" {
			rw.CollectionName = a.Collection
		}
		if a.LastSync.IsZero() {
			rw.LastSyncText = "jamais"
		} else {
			rw.LastSyncText = a.LastSync.In(s.Engine.Loc).Format("02/01/2006 15:04")
		}
		p.Accounts = append(p.Accounts, rw)
	}
	trash, err := s.Store.Trash(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	p.Trash = s.trashRows(trash, list)
	if p.Claude, err = s.claudeRows(ctx); err != nil {
		s.fail(w, err)
		return
	}
	p.ConnectorURL = s.resource()
	s.render(w, http.StatusOK, "home", p)
}

// trashRows are the trash's events and dates, the last deleted first.
func (s *Server) trashRows(items []store.Item, list []store.Account) []trashRow {
	names := map[int64]string{}
	for _, a := range list {
		names[a.ID] = a.Name
	}
	var rows []trashRow
	for _, it := range items {
		title, start, series := "", (*ical.Prop)(nil), false
		if cal, err := ics.Parse(it.SourceData); err == nil {
			for _, ev := range ics.Events(cal) {
				if ics.RecurrenceID(ev) == "" {
					title, _ = ev.Props.Text(ical.PropSummary)
					start = ev.Props.Get(ical.PropDateTimeStart)
					series = ev.Props.Get(ical.PropRecurrenceRule) != nil || ev.Props.Get(ical.PropRecurrenceDates) != nil
					break
				}
			}
		}
		if title == "" {
			title = "(sans titre)"
		}
		base := trashRow{AccountID: it.AccountID, UID: it.UID, Title: title, Account: names[it.AccountID]}
		if !it.Trashed.IsZero() {
			r := base
			if start != nil {
				r.When = s.dateText(start, start.Value)
			}
			if series {
				r.When = strings.TrimPrefix(r.When+" · se répète", " · ")
			}
			r.at, r.Deleted = it.Trashed, it.Trashed.In(s.Engine.Loc).Format("02/01/2006")
			rows = append(rows, r)
		}
		for _, d := range it.Removed {
			r := base
			r.Line = d.Line
			if p, err := ics.Line(d.Line); err == nil {
				if v := ics.Values(p); len(v) > 0 {
					r.When = s.dateText(p, v[0]) + " · une date de la série"
				}
			}
			r.at, r.Deleted = d.At, d.At.In(s.Engine.Loc).Format("02/01/2006")
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.After(rows[j].at) })
	return rows
}

var weekdays = [...]string{"dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."}

// dateText says one date value as the page does, "mar. 10/11/2026 14:00" or the day alone: in the page's zone when
// its own is known, else as written.
func (s *Server) dateText(p *ical.Prop, v string) string {
	key := ics.Instant(p, v)
	if d, err := time.Parse("20060102", key); err == nil {
		return weekdays[d.Weekday()] + " " + d.Format("02/01/2006")
	}
	if t, err := time.Parse("20060102T150405Z", key); err == nil {
		t = t.In(s.Engine.Loc)
		return weekdays[t.Weekday()] + " " + t.Format("02/01/2006 15:04")
	}
	if t, err := time.Parse("20060102T150405", key[strings.LastIndex(key, ":")+1:]); err == nil {
		return weekdays[t.Weekday()] + " " + t.Format("02/01/2006 15:04")
	}
	return v
}

func (s *Server) provider(ctx context.Context, kind string, list []store.Account) (provider, error) {
	v := provider{Kind: kind, Label: kindLabel(kind), EnvPrefix: strings.ToUpper(kind), FromEnv: s.FromEnv[kind],
		BaseURL: s.BaseURL, CallbackURL: s.BaseURL + CallbackPath}
	for _, a := range list {
		if a.Kind == kind {
			v.Accounts++
		}
	}
	cur := s.Kinds.Provider(kind)
	v.On = cur != nil
	if v.FromEnv {
		if cur != nil {
			v.ClientID, v.Tenant = cur.Config.ClientID, cur.Tenant
		}
		return v, nil
	}
	saved, err := s.Kinds.Saved(ctx, kind)
	v.ClientID, v.Tenant, v.HasSecret = saved.ClientID, saved.Tenant, saved.ClientSecret != ""
	return v, err
}

// saveProvider keeps the OAuth client typed for a kind and puts it in place at once. A blank secret keeps the one
// kept; a kind the environment sets is left alone (it would win again at the next start).
func (s *Server) saveProvider(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kind := r.PathValue("kind")
	if kind != "google" && kind != "microsoft" {
		s.problem(w, "Connexion inconnue : "+kind+".")
		return
	}
	if s.FromEnv[kind] {
		s.problem(w, "La connexion "+kindLabel(kind)+" est réglée dans l'environnement du serveur ("+strings.ToUpper(kind)+
			"_CLIENT_ID…, deploy/.env) : change-la là, puis redéploie.")
		return
	}
	old, err := s.Kinds.Saved(ctx, kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	p := accounts.ProviderSettings{ClientID: strings.TrimSpace(r.PostFormValue("client_id")),
		ClientSecret: strings.TrimSpace(r.PostFormValue("client_secret"))}
	if kind == "microsoft" {
		if p.Tenant = strings.ToLower(strings.TrimSpace(r.PostFormValue("tenant"))); p.Tenant == "" {
			p.Tenant = "common"
		}
	}
	if p.ClientSecret == "" {
		p.ClientSecret = old.ClientSecret
	}
	if err := p.Check(kind); err != nil {
		s.problem(w, err.Error())
		return
	}
	// Accounts signed in through another client (or directory) can't refresh their tokens through this one.
	cur := s.Kinds.Provider(kind)
	moved := cur != nil && (cur.Config.ClientID != p.ClientID || kind == "microsoft" && cur.Tenant != p.Tenant)
	if err := s.Kinds.Save(ctx, kind, p, s.BaseURL+CallbackPath); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("sign-in set on the accounts page", "kind", kind)
	ok := kind + "-set"
	if moved {
		list, err := s.Store.Accounts(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		if slices.ContainsFunc(list, func(a store.Account) bool { return a.Kind == kind }) {
			ok += "-reconnect"
		}
	}
	http.Redirect(w, r, "/?ok="+ok, http.StatusSeeOther)
}

// collections are mine that hold events, by name.
func (s *Server) collections(ctx context.Context) ([]davis.Calendar, error) {
	all, err := s.Engine.Davis.Calendars(ctx, s.Engine.Home)
	if err != nil {
		return nil, err
	}
	var out []davis.Calendar
	for _, c := range all {
		for _, comp := range c.Components {
			if comp == "VEVENT" {
				out = append(out, c)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName) })
	return out, nil
}

// --- adding accounts

// addFeed adds a published calendar. My address in it is optional (a timetable invites nobody: it only says which
// ATTENDEE is me, to show my answers), and several feeds may share it (the school's Outlook and its intranet): a
// feed is told apart by its link.
func (s *Server) addFeed(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	address := strings.TrimSpace(r.PostFormValue("address"))
	link := strings.TrimSpace(r.PostFormValue("url"))
	u, err := url.Parse(link)
	switch {
	case name == "" || link == "":
		s.problem(w, "Donne un nom et le lien du calendrier.")
		return
	case err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "webcal") || u.Host == "":
		s.problem(w, "Le lien doit commencer par https://, http:// ou webcal://.")
		return
	case address != "" && !strings.Contains(address, "@"):
		s.problem(w, "L'adresse doit être une adresse mail.")
		return
	}
	if s.sameFeed(r.Context(), link, w) {
		return
	}
	conf := fmt.Sprintf(`{"url":%q}`, link)
	if _, err := s.Engine.AddAccount(r.Context(), store.Account{Kind: "ics", Name: name, Address: address, Config: conf},
		r.PostFormValue("collection")); err != nil {
		s.problem(w, "Impossible d'ajouter le compte : "+err.Error())
		return
	}
	s.Engine.Wake()
	http.Redirect(w, r, "/?ok=added", http.StatusSeeOther)
}

// sameFeed refuses a second account for the same feed (its events would be copied twice). webcal:// is https://.
func (s *Server) sameFeed(ctx context.Context, link string, w http.ResponseWriter) bool {
	list, err := s.Store.Accounts(ctx)
	if err != nil {
		s.fail(w, err)
		return true
	}
	for _, a := range list {
		var conf accounts.FeedConfig
		if a.Kind == "ics" && json.Unmarshal([]byte(a.Config), &conf) == nil && feedKey(conf.URL) == feedKey(link) {
			s.problem(w, "Ce calendrier est déjà ajouté ("+a.Name+").")
			return true
		}
	}
	return false
}

// feedKey is a feed's link as compared: scheme and host in lower case, webcal:// as https://.
func feedKey(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return strings.TrimSpace(link)
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if u.Scheme == "webcal" {
		u.Scheme = "https"
	}
	return u.String()
}

// duplicate refuses a second account for the same address of the same kind (its events would be copied twice).
func (s *Server) duplicate(ctx context.Context, kind, address string, w http.ResponseWriter) bool {
	list, err := s.Store.Accounts(ctx)
	if err != nil {
		s.fail(w, err)
		return true
	}
	for _, a := range list {
		if a.Kind == kind && ics.Address(a.Address) == ics.Address(address) {
			s.problem(w, "Ce compte est déjà ajouté ("+a.Name+").")
			return true
		}
	}
	return false
}

// connect sends the browser to the provider's sign-in, for a new account or one to sign in again.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	p := s.Kinds.Provider(kind)
	if p == nil {
		s.problem(w, "La connexion "+kindLabel(kind)+" n'est pas configurée sur ce serveur.")
		return
	}
	in := signIn{Kind: kind, Verifier: oauth2.GenerateVerifier(), Started: time.Now()}
	if id := r.PostFormValue("account"); id != "" {
		n, _ := strconv.ParseInt(id, 10, 64)
		a, err := s.Store.Account(r.Context(), n)
		if err != nil || a.Kind != kind {
			s.problem(w, "Ce compte n'existe plus.")
			return
		}
		in.AccountID = a.ID
	} else {
		in.Name = strings.TrimSpace(r.PostFormValue("name")) // none: the account's address, once known
		c, err := s.Engine.Collection(r.Context(), r.PostFormValue("collection"))
		if err != nil {
			s.problem(w, "Choisis une collection : "+err.Error())
			return
		}
		in.Collection = c.Href
	}
	state := oauth2.GenerateVerifier() // 32 random bytes, as good a state as any
	s.mu.Lock()
	for k, v := range s.signIns {
		if time.Since(v.Started) > signInLife {
			delete(s.signIns, k)
		}
	}
	s.signIns[state] = in
	s.mu.Unlock()
	http.Redirect(w, r, p.AuthURL(state, in.Verifier), http.StatusSeeOther)
}

func (s *Server) takeSignIn(state string) (signIn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.signIns[state]
	delete(s.signIns, state)
	return in, ok && time.Since(in.Started) <= signInLife
}

// callback is where the provider sends the browser back: the account is added, or signed in again.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	in, ok := s.takeSignIn(q.Get("state"))
	if !ok {
		s.problem(w, "Ce lien de connexion a expiré ou a déjà servi : recommence depuis la page des comptes.")
		return
	}
	if e := q.Get("error"); e != "" {
		msg := q.Get("error_description")
		if msg == "" {
			msg = e
		}
		s.problem(w, kindLabel(in.Kind)+" a refusé la connexion : "+msg)
		return
	}
	p := s.Kinds.Provider(in.Kind)
	tok, err := p.Exchange(ctx, q.Get("code"), in.Verifier)
	if err != nil {
		s.Log.Warn("sign-in failed", "kind", in.Kind, "err", err)
		s.problem(w, "La connexion n'a pas abouti : "+err.Error())
		return
	}
	address, err := s.Kinds.Address(ctx, in.Kind, tok)
	if err != nil {
		s.problem(w, "Impossible de connaître l'adresse de ce compte : "+err.Error())
		return
	}
	config := oauth.Stored{Token: tok}.Encode()

	if in.AccountID != 0 {
		a, err := s.Store.Account(ctx, in.AccountID)
		if err != nil {
			s.problem(w, "Ce compte n'existe plus.")
			return
		}
		if ics.Address(a.Address) != ics.Address(address) {
			s.problem(w, "Tu t'es connecté avec "+address+", mais ce compte est celui de "+a.Address+".")
			return
		}
		if err := s.Store.SetAccountConfig(ctx, a.ID, config); err != nil {
			s.fail(w, err)
			return
		}
		if err := s.Store.RetryFailedAnswers(ctx, a.ID); err != nil {
			s.fail(w, err)
			return
		}
		s.Log.Info("account signed in again", "account", a.ID)
		s.Engine.Wake()
		http.Redirect(w, r, "/?ok=reconnected", http.StatusSeeOther)
		return
	}
	if s.duplicate(ctx, in.Kind, address, w) {
		return
	}
	if in.Name == "" {
		in.Name = address
	}
	if _, err := s.Engine.AddAccount(ctx, store.Account{Kind: in.Kind, Name: in.Name, Address: address, Config: config},
		in.Collection); err != nil {
		s.problem(w, "Impossible d'ajouter le compte : "+err.Error())
		return
	}
	s.Engine.Wake()
	http.Redirect(w, r, "/?ok=added", http.StatusSeeOther)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.Store.Account(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.problem(w, "Ce compte n'existe plus.")
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	events := r.PostFormValue("events")
	if events != "delete" && events != "keep" {
		s.problem(w, "Dis ce que deviennent ses événements : supprimés de Davis, ou gardés tels quels.")
		return
	}
	if err := s.Engine.RemoveAccount(r.Context(), a, events == "delete"); err != nil {
		s.problem(w, "Impossible de retirer le compte : "+err.Error())
		return
	}
	http.Redirect(w, r, "/?ok=removed", http.StatusSeeOther)
}

// restore takes an event, or one date of its series, out of the trash: written again in my calendar, up to date.
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PostFormValue("account"), 10, 64)
	err := s.Engine.Restore(r.Context(), id, r.PostFormValue("uid"), r.PostFormValue("date"))
	if errors.Is(err, store.ErrNotFound) {
		s.problem(w, "Cet événement n'est plus là : son organisateur l'a supprimé, ou son compte a été retiré.")
		return
	} else if err != nil {
		s.problem(w, "Impossible de le restaurer : "+err.Error())
		return
	}
	http.Redirect(w, r, "/?ok=restored#corbeille", http.StatusSeeOther)
}

// --- rendering

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	s.renderAny(w, status, name, p)
}

func (s *Server) renderAny(w http.ResponseWriter, status int, name string, p any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages.ExecuteTemplate(w, name, p); err != nil {
		s.Log.Error("page", "template", name, "err", err)
	}
}

// problem is something I can fix: said on its own page, with a way back.
func (s *Server) problem(w http.ResponseWriter, msg string) {
	s.render(w, http.StatusBadRequest, "error", page{Error: msg, Back: "/"})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("accounts page", "err", err)
	s.render(w, http.StatusInternalServerError, "error", page{Error: "Erreur du serveur : " + err.Error(), Back: "/"})
}

func kindLabel(kind string) string {
	switch kind {
	case "google":
		return "Google"
	case "microsoft":
		return "Microsoft"
	case "ics":
		return "Flux ICS"
	}
	return kind
}
