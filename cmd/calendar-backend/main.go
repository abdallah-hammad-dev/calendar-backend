// Command calendar-backend mirrors external accounts into Davis and watches the mirrors for my answers.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/adapter/icsfeed"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/config"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/davis"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/mirror"
	"github.com/abdallah-hammad-dev/calendar-backend/internal/store"
)

const usage = `usage: calendar-backend [-env FILE] COMMAND

Commands:
  run         pass every DAVIS_INTERVAL (accounts every SOURCE_INTERVAL) until stopped
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
	e := &mirror.Engine{Store: st, Davis: dc, Me: p.Email(), Home: p.Home, Vault: cfg.Vault, Loc: cfg.Location,
		Adapter: adapterFor, Log: log}

	switch cmd {
	case "run":
		log.Info("running", "davis", cfg.DavisURL, "user", cfg.DavisUser, "me", e.Me,
			"davis_every", cfg.DavisEvery, "source_every", cfg.SourceEvery)
		return e.Run(ctx, cfg.DavisEvery, cfg.SourceEvery)
	case "once":
		return e.Pass(ctx, true)
	case "add-feed":
		fs := flag.NewFlagSet("add-feed", flag.ExitOnError)
		name := fs.String("name", "", "the account's name")
		address := fs.String("address", "", "my address on that account (the one invited)")
		url := fs.String("url", "", "the feed: https://…, webcal://… or file:///…")
		collection := fs.String("collection", "", "the collection of mine its events go into, by name or href")
		fs.Parse(args)
		if *name == "" || *address == "" || *url == "" || *collection == "" {
			return errors.New("add-feed: -name, -address, -url and -collection are required")
		}
		conf, _ := json.Marshal(feedConfig{URL: *url})
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

type feedConfig struct {
	URL string `json:"url"`
}

func adapterFor(a store.Account) (adapter.Adapter, error) {
	switch a.Kind {
	case "ics":
		var c feedConfig
		if err := json.Unmarshal([]byte(a.Config), &c); err != nil {
			return nil, fmt.Errorf("account %d: bad config: %w", a.ID, err)
		}
		return &icsfeed.Feed{URL: c.URL}, nil
	}
	return nil, fmt.Errorf("account %d: unknown kind %q", a.ID, a.Kind)
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
