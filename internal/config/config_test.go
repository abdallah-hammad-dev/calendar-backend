package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backend.env")
	os.WriteFile(path, []byte("# comment\nDAVIS_USER=abd\nDAVIS_PASSWORD=\"from file\"\nVAULT='Mon coffre'\nSOURCE_INTERVAL=10m\n"), 0o600)
	t.Setenv("DAVIS_PASSWORD", "from env")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DavisUser != "abd" || c.DavisPassword != "from env" || c.Vault != "Mon coffre" {
		t.Fatalf("%+v", c)
	}
	if c.SourceEvery != 10*time.Minute || c.DavisEvery != 30*time.Second || c.Location.String() != "Europe/Paris" {
		t.Fatalf("%+v", c)
	}
	if c.DavisURL != "http://127.0.0.1:9000/dav/" || c.DBPath != "data/backend.db" {
		t.Fatalf("%+v", c)
	}
	if c.Listen != "127.0.0.1:8080" || c.BaseURL != "http://localhost:8080" || c.MicrosoftTenant != "common" {
		t.Fatalf("%+v", c)
	}
}

func TestLoadAccountsPage(t *testing.T) {
	t.Setenv("DAVIS_USER", "abd")
	t.Setenv("DAVIS_PASSWORD", "x")
	t.Setenv("BASE_URL", "https://comptes.example.fr/")
	t.Setenv("GOOGLE_CLIENT_ID", "id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "secret")
	c, err := Load("")
	if err != nil || c.BaseURL != "https://comptes.example.fr" {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("GOOGLE_CLIENT_SECRET", "")
	if _, err := Load(""); err == nil {
		t.Fatal("a client id without its secret")
	}
	t.Setenv("GOOGLE_CLIENT_SECRET", "secret")
	t.Setenv("BASE_URL", "comptes.example.fr")
	if _, err := Load(""); err == nil {
		t.Fatal("a BASE_URL without its scheme")
	}
}

func TestLoadErrors(t *testing.T) {
	t.Setenv("DAVIS_USER", "")
	t.Setenv("TIMEZONE", "Mars/Olympus")
	t.Setenv("DAVIS_INTERVAL", "10ms")
	if _, err := Load(""); err == nil {
		t.Fatal("no error")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("no error without credentials")
	}
}
