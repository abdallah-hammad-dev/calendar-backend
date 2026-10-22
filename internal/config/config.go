// Package config reads the backend's settings from the environment, after an optional env file (KEY=value lines)
// that the real environment overrides.
package config

import (
	"bufio"
	"errors"
	"fmt"
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
	}
	var errs []error
	if c.DavisUser == "" || c.DavisPassword == "" {
		errs = append(errs, errors.New("DAVIS_USER and DAVIS_PASSWORD are required"))
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
