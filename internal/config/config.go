// Package config reads the backend's settings from the environment, after an optional env file (KEY=value lines)
// that the real environment overrides.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	DavisURL      string         // DAVIS_URL, e.g. http://127.0.0.1:9000/dav/
	DavisUser     string         // DAVIS_USER: the Davis user the mirrors are written for
	DavisPassword string         // DAVIS_PASSWORD
	DBPath        string         // DB_PATH
	Vault         string         // VAULT: Obsidian vault name in obsidian:// links ("" = no vault parameter)
	Location      *time.Location // TIMEZONE (IANA): where times given in UTC are written
	DavisEvery    time.Duration  // DAVIS_INTERVAL: how often the mirror collections are read (answers)
	SourceEvery   time.Duration  // SOURCE_INTERVAL: how often the accounts are read

	Listen  string // LISTEN: the accounts page's address ("" = no page)
	BaseURL string // BASE_URL: the page's public address; BASE_URL/oauth/callback is the redirect URI

	GoogleClientID        string // GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET: the Google Cloud project's OAuth client
	GoogleClientSecret    string //   ("" = no Google accounts)
	MicrosoftClientID     string // MICROSOFT_CLIENT_ID, MICROSOFT_CLIENT_SECRET: the Entra app registration
	MicrosoftClientSecret string //   ("" = no Microsoft accounts)
	MicrosoftTenant       string // MICROSOFT_TENANT: "common" (personal and any organization's accounts)
}

// Load reads envFile if it exists ("" = none), then the environment.
func Load(envFile string) (Config, error) {
	file := map[string]string{}
	if envFile != "" {
		var err error
		if file, err = readEnvFile(envFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
	}
	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if v, ok := file[key]; ok {
			return v
		}
		return def
	}

	c := Config{
		DavisURL:      get("DAVIS_URL", "http://127.0.0.1:9000/dav/"),
		DavisUser:     get("DAVIS_USER", ""),
		DavisPassword: get("DAVIS_PASSWORD", ""),
		DBPath:        get("DB_PATH", "data/backend.db"),
		Vault:         get("VAULT", ""),

		Listen:                get("LISTEN", "127.0.0.1:8080"),
		BaseURL:               strings.TrimRight(get("BASE_URL", "http://localhost:8080"), "/"),
		GoogleClientID:        get("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret:    get("GOOGLE_CLIENT_SECRET", ""),
		MicrosoftClientID:     get("MICROSOFT_CLIENT_ID", ""),
		MicrosoftClientSecret: get("MICROSOFT_CLIENT_SECRET", ""),
		MicrosoftTenant:       get("MICROSOFT_TENANT", "common"),
	}
	var errs []error
	if c.DavisUser == "" || c.DavisPassword == "" {
		errs = append(errs, errors.New("DAVIS_USER and DAVIS_PASSWORD are required"))
	}
	if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
		errs = append(errs, fmt.Errorf("BASE_URL %q: want the page's address, like https://comptes.example.fr", c.BaseURL))
	}
	for _, pair := range [][3]string{{"GOOGLE", c.GoogleClientID, c.GoogleClientSecret},
		{"MICROSOFT", c.MicrosoftClientID, c.MicrosoftClientSecret}} {
		if (pair[1] == "") != (pair[2] == "") {
			errs = append(errs, fmt.Errorf("%s_CLIENT_ID and %s_CLIENT_SECRET go together", pair[0], pair[0]))
		}
	}
	tz := get("TIMEZONE", "Europe/Paris")
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "Local" || tz == "" {
		errs = append(errs, fmt.Errorf("TIMEZONE %q: not an IANA zone name", tz))
	}
	c.Location = loc
	for _, d := range []struct {
		key, def string
		to       *time.Duration
	}{{"DAVIS_INTERVAL", "30s", &c.DavisEvery}, {"SOURCE_INTERVAL", "5m", &c.SourceEvery}} {
		v, err := time.ParseDuration(get(d.key, d.def))
		if err != nil || v < time.Second {
			errs = append(errs, fmt.Errorf("%s: want a duration of 1s or more, like %s", d.key, d.def))
		}
		*d.to = v
	}
	return c, errors.Join(errs...)
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want KEY=value", path, n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}
