package main

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRealtimeAcrossReplicas(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL TEST_DATABASE_URL")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	old := db
	db = database
	defer func() { db = old }()
	if err = migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Exec("TRUNCATE realtime_events"); err != nil {
		t.Fatal(err)
	}
	a := make(chan WSEvent, 64)
	b := make(chan WSEvent, 64)
	first, err := newRealtimeBus(database, dsn, func(e WSEvent) { a <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := newRealtimeBus(database, dsn, func(e WSEvent) { b <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	const count = 20
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- first.publish(context.Background(), WSEvent{Type: "new_message", Message: &Message{ID: i + 1, ChannelID: 1, Content: strings.Repeat("E1:ciphertext", 2000)}})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, events := range []chan WSEvent{a, b} {
		seen := make(map[int]bool)
		timeout := time.After(10 * time.Second)
		for len(seen) < count {
			select {
			case e := <-events:
				if e.Type != "new_message" || e.Message == nil || len(e.Message.Content) < 8000 || e.Message.FileData != "" {
					t.Fatal("event metadata changed")
				}
				if seen[e.Message.ID] {
					t.Fatal("duplicate delivery")
				}
				seen[e.Message.ID] = true
			case <-timeout:
				t.Fatalf("missing events: got %d of %d", len(seen), count)
			}
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for (!first.healthy() || !second.healthy()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !first.healthy() || !second.healthy() {
		t.Fatal("both replicas must be ready")
	}
	var n int
	if err = database.QueryRow("SELECT count(*) FROM realtime_events").Scan(&n); err != nil || n != count {
		t.Fatal("durable events missing")
	}
}
