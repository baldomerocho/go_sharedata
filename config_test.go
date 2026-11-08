package main

import (
	"net/url"
	"testing"
)

func TestDatabaseURLPrecedence(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://explicit:secret@db:5432/isolated?sslmode=require")
	t.Setenv("DB_HOST", "ignored")
	if got := databaseURL(); got != "postgres://explicit:secret@db:5432/isolated?sslmode=require" {
		t.Fatal("DATABASE_URL must take precedence")
	}
}

func TestDatabaseURLEscapesCredentials(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "::1")
	t.Setenv("DB_PORT", "5433")
	t.Setenv("DB_USER", "app@tenant")
	password := "a'b c@d:sslmode=disable&x=y"
	t.Setenv("DB_PASSWORD", password)
	t.Setenv("DB_NAME", "sharedata_dev")
	t.Setenv("DB_SSLMODE", "require")
	u, err := url.Parse(databaseURL())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := u.User.Password()
	if p != password || u.User.Username() != "app@tenant" || u.Host != "[::1]:5433" || u.Path != "/sharedata_dev" || u.Query().Get("sslmode") != "require" {
		t.Fatal("database configuration did not round-trip safely")
	}
}
