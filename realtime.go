package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

// Los eventos contienen metadatos cifrados, nunca el adjunto. PostgreSQL guarda
// el evento y su notificación en la misma transacción para todas las réplicas.
type realtimeBus struct {
	db        *sql.DB
	listener  *pq.Listener
	deliver   func(WSEvent)
	cursor    int64
	connected atomic.Bool
	ready     atomic.Bool
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

func newRealtimeBus(database *sql.DB, dsn string, deliver func(WSEvent)) (*realtimeBus, error) {
	b := &realtimeBus{db: database, deliver: deliver, done: make(chan struct{})}
	if err := database.QueryRow("SELECT COALESCE(MAX(id),0) FROM realtime_events").Scan(&b.cursor); err != nil {
		return nil, err
	}
	b.listener = pq.NewListener(dsn, time.Second, 10*time.Second, func(event pq.ListenerEventType, _ error) {
		b.connected.Store(event == pq.ListenerEventConnected || event == pq.ListenerEventReconnected)
	})
	if err := b.listener.Listen("sharedata_events"); err != nil {
		b.listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	go b.run(ctx)
	return b, nil
}

func (b *realtimeBus) healthy() bool { return b.connected.Load() && b.ready.Load() }

func (b *realtimeBus) run(ctx context.Context) {
	defer close(b.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for {
		if err := b.catchUp(ctx); err != nil {
			b.ready.Store(false)
			log.Print("No se pudieron recuperar eventos compartidos")
		} else {
			b.ready.Store(true)
		}
		select {
		case <-ctx.Done():
			return
		case <-b.listener.Notify:
		case <-ticker.C:
		case <-cleanup.C:
			_, _ = b.db.ExecContext(ctx, "DELETE FROM realtime_events WHERE created_at < NOW() - INTERVAL '1 day'")
		}
	}
}

func (b *realtimeBus) catchUp(ctx context.Context) error {
	for {
		rows, err := b.db.QueryContext(ctx, "SELECT id,payload FROM realtime_events WHERE id > $1 ORDER BY id LIMIT 100", b.cursor)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var id int64
			var raw []byte
			var ev WSEvent
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal(raw, &ev); err != nil {
				rows.Close()
				return err
			}
			b.deliver(ev)
			b.cursor = id
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count < 100 {
			return nil
		}
	}
}

func (b *realtimeBus) publish(ctx context.Context, event WSEvent) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serializa el orden de commit: un ID mayor no puede adelantarse a otro
	// todavía sin confirmar y hacer que los lectores omitan un evento.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('sharedata_realtime_publish'))"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "WITH e AS (INSERT INTO realtime_events(payload) VALUES($1) RETURNING id) SELECT pg_notify('sharedata_events',id::text) FROM e", string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *realtimeBus) close() {
	b.closeOnce.Do(func() { b.cancel(); _ = b.listener.Close(); <-b.done })
}
