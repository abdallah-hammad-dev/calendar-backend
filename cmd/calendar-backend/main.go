// Command calendar-backend mirrors external accounts into Davis, relays my answers to their invitations, and serves
// the accounts page.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"
	// The zone database inside the binary: a minimal container has none, and events' zones would then go out
	// without their VTIMEZONE.
	_ "time/tzdata"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/accounts"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/icsfeed"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/config"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/connector"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/mirror"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/vault"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/web"
)

const usage = `usage: calendar-backend [-env FILE] COMMAND

Commands:
  run         the accounts page on LISTEN, and a pass every DAVIS_INTERVAL (accounts every SOURCE_INTERVAL),
              until stopped
  once        one pass, accounts included
  accounts    list the accounts and how their last pass went
  add-feed    add an ICS feed account (read-only): -name, -address, -url, -collection (one of mine, by name or href)
  remove      remove an account: -id, and -delete-events or -keep-events (what becomes of its events on Davis)

Settings come from the environment, after FILE (default backend.env, if it exists): see backend.env.example.
`

func main() {
	envFile := flag.String("env", "backend.env", "env file read before the environment (missing = skipped)")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *envFile, flag.Arg(0), flag.Args()[1:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "calendar-backend:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, envFile, cmd string, args []string) error {
	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if cmd == "accounts" {
		return listAccounts(ctx, st)
	}

	dc, err := davis.New(cfg.DavisURL, cfg.DavisUser, cfg.DavisPassword, nil)
	if err != nil {
		return err
	}
	p, err := dc.Discover(ctx)
	if err != nil {
		return err
	}
	if p.Email() == "" {
		return fmt.Errorf("Davis user %q has no email: set one in the Davis dashboard (it is the address invitations are shown for)", cfg.DavisUser)
	}
	kinds := &accounts.Kinds{Store: st}
	// The environment's OAuth clients win; a kind it leaves empty takes the one entered on the accounts page.
	fromEnv, err := kinds.Setup(ctx, map[string]accounts.ProviderSettings{
		"google":    {ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret},
		"microsoft": {ClientID: cfg.MicrosoftClientID, ClientSecret: cfg.MicrosoftClientSecret, Tenant: cfg.MicrosoftTenant},
	}, cfg.BaseURL+web.CallbackPath)
	if err != nil {
		return err
	}
	e := &mirror.Engine{Store: st, Davis: dc, Me: p.Email(), Home: p.Home, Vault: cfg.Vault, Loc: cfg.Location,
		Adapter: kinds.Adapter, Log: log}

	switch cmd {
	case "run":
		log.Info("running", "davis", cfg.DavisURL, "user", cfg.DavisUser, "me", e.Me,
			"davis_every", cfg.DavisEvery, "source_every", cfg.SourceEvery, "page", cfg.BaseURL,
			"google", kinds.Provider("google") != nil, "google_from_env", fromEnv["google"],
			"microsoft", kinds.Provider("microsoft") != nil, "microsoft_from_env", fromEnv["microsoft"],
			"connector", cfg.BaseURL+web.MCPPath, "vault", cfg.VaultDir != "")
		if cfg.Listen == "" {
			return e.Run(ctx, cfg.DavisEvery, cfg.SourceEvery)
		}
		conn := &connector.Connector{Davis: dc, Home: p.Home, Me: p.Email(), Loc: cfg.Location, Store: st,
			TasksFolder: cfg.TasksFolder, EventsFolder: cfg.EventsFolder, EventDateFormat: cfg.EventDateFormat,
			Wake: e.Wake, Log: log}
		if cfg.VaultDir != "" {
			v, err := vault.Open(cfg.VaultDir)
			if err != nil {
				return fmt.Errorf("VAULT_DIR: %w", err)
			}
			defer v.Close()
			conn.Vault = v
		}
		page := &web.Server{Engine: e, Store: st, Kinds: kinds, FromEnv: fromEnv, BaseURL: cfg.BaseURL,
			User: cfg.DavisUser, Password: cfg.DavisPassword, Log: log, MCP: conn.Handler()}
		h, err := page.Handler(ctx)
		if err != nil {
			return err
		}
		// Every connection has an end (security review, 2026-11-05: only the headers had one, so slow clients could
		// hold connections and memory, an MCP body being up to 8 MB). Writing allows the longest tool call: a
		// connector call can make several Davis requests, each given up after 2 minutes.
		srv := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute,
			WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
		served := make(chan error, 1)
		go func() { served <- srv.ListenAndServe() }()
		go func() {
			<-ctx.Done()
			shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(shut)
		}()
		ran := make(chan error, 1)
		go func() { ran <- e.Run(ctx, cfg.DavisEvery, cfg.SourceEvery) }()
		select {
		case err := <-served:
			if !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("accounts page on %s: %w", cfg.Listen, err)
			}
			return <-ran
		case err := <-ran:
			return err
		}
	case "once":
		return e.Pass(ctx, true)
	case "add-feed":
		fs := flag.NewFlagSet("add-feed", flag.ExitOnError)
		name := fs.String("name", "", "the account's name")
		address := fs.String("address", "", "my address in it, if I'm invited there (optional)")
		url := fs.String("url", "", "the feed: https://… or webcal://…")
		collection := fs.String("collection", "", "the collection of mine its events go into, by name or href")
		fs.Parse(args)
		if *name == "" || *url == "" || *collection == "" {
			return errors.New("add-feed: -name, -url and -collection are required")
		}
		if err := icsfeed.CheckURL(*url); err != nil {
			return err
		}
		conf, _ := json.Marshal(accounts.FeedConfig{URL: *url})
		a, err := e.AddAccount(ctx, store.Account{Kind: "ics", Name: *name, Address: *address, Config: string(conf)}, *collection)
		if err != nil {
			return err
		}
		fmt.Printf("account %d added, its events go into %s\n", a.ID, a.Collection)
		return nil
	case "remove":
		fs := flag.NewFlagSet("remove", flag.ExitOnError)
		id := fs.Int64("id", 0, "the account's id (see: accounts)")
		del := fs.Bool("delete-events", false, "delete its events from Davis")
		keep := fs.Bool("keep-events", false, "leave its events on Davis (no longer updated)")
		fs.Parse(args)
		if *del == *keep {
			return errors.New("remove: say what becomes of its events: -delete-events or -keep-events")
		}
		a, err := st.Account(ctx, *id)
		if err != nil {
			return fmt.Errorf("account %d: %w", *id, err)
		}
		if err := e.RemoveAccount(ctx, a, *del); err != nil {
			return err
		}
		if *del {
			fmt.Printf("account %d removed, its events deleted from %s\n", a.ID, a.Collection)
		} else {
			fmt.Printf("account %d removed; its events stay in %s\n", a.ID, a.Collection)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q (see -h)", cmd)
}

func listAccounts(ctx context.Context, st *store.Store) error {
	accounts, err := st.Accounts(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tKIND\tNAME\tADDRESS\tCOLLECTION\tLAST SYNC\tERROR")
	for _, a := range accounts {
		last := "never"
		if !a.LastSync.IsZero() {
			last = a.LastSync.Local().Format(time.DateTime)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", strconv.FormatInt(a.ID, 10), a.Kind, a.Name, a.Address,
			a.Collection, last, a.LastError)
	}
	return w.Flush()
}
