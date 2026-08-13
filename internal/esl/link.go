// SPDX-License-Identifier: Apache-2.0

package esl

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Reconnect backoff bounds. A switch restart should be picked up quickly
// without hammering a switch that is genuinely down.
const (
	backoffMin = 500 * time.Millisecond
	backoffMax = 30 * time.Second
)

// Link keeps one Client connected, reconnecting as needed, and republishes
// events on a stable channel so consumers survive reconnects.
type Link struct {
	addr     string
	password string
	// subscriptions are re-applied after every successful connect.
	subscriptions []string

	events chan *Event

	client atomic.Pointer[Client]
	up     atomic.Bool

	mu        sync.Mutex
	onConnect []func(context.Context)
	onLost    []func()
}

// NewLink builds a Link. Call Run to start connecting.
func NewLink(addr, password string, subscriptions []string) *Link {
	return &Link{
		addr:          addr,
		password:      password,
		subscriptions: subscriptions,
		events:        make(chan *Event, 1024),
	}
}

// Events is the stable event stream across reconnects.
func (l *Link) Events() <-chan *Event { return l.events }

// IsUp reports whether a healthy connection exists right now.
func (l *Link) IsUp() bool { return l.up.Load() }

// OnConnect registers a hook run after each successful connect and
// subscription, e.g. state reconciliation.
func (l *Link) OnConnect(fn func(context.Context)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onConnect = append(l.onConnect, fn)
}

// OnLost registers a hook run when a connection drops.
func (l *Link) OnLost(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onLost = append(l.onLost, fn)
}

// Run maintains the connection until ctx ends. It returns only on ctx done.
func (l *Link) Run(ctx context.Context) {
	defer close(l.events)

	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}

		client, err := Dial(ctx, l.addr, l.password)
		if err != nil {
			slog.WarnContext(ctx, "esl connect failed", "addr", l.addr, "error", err,
				"retryIn", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}

		if err := client.Subscribe(l.subscriptions...); err != nil {
			slog.WarnContext(ctx, "esl subscribe failed", "error", err)
			client.Close()
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}

		l.client.Store(client)
		l.up.Store(true)
		backoff = backoffMin
		slog.InfoContext(ctx, "esl connected", "addr", l.addr)

		l.runHooks(ctx)
		l.pump(ctx, client)

		l.up.Store(false)
		l.client.Store(nil)
		client.Close()
		l.runLostHooks()
		slog.WarnContext(ctx, "esl disconnected", "addr", l.addr)

		if !sleepCtx(ctx, backoff) {
			return
		}
	}
}

// pump forwards events until the client dies or ctx ends.
func (l *Link) pump(ctx context.Context, client *Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-client.Events():
			if !open {
				return
			}
			select {
			case l.events <- ev:
			case <-ctx.Done():
				return
			default:
				// Never block the reader: drop oldest, keep the newest.
				select {
				case <-l.events:
				default:
				}
				select {
				case l.events <- ev:
				default:
				}
			}
		}
	}
}

// API runs a blocking api command, or reports ErrDown when disconnected.
func (l *Link) API(cmd string) (string, error) {
	client := l.client.Load()
	if client == nil {
		return "", ErrDown
	}
	return client.API(cmd)
}

// BgAPI runs a background command, or reports ErrDown when disconnected.
func (l *Link) BgAPI(cmd string) (string, error) {
	client := l.client.Load()
	if client == nil {
		return "", ErrDown
	}
	return client.BgAPI(cmd)
}

func (l *Link) runHooks(ctx context.Context) {
	l.mu.Lock()
	hooks := slices.Clone(l.onConnect)
	l.mu.Unlock()
	for _, fn := range hooks {
		fn(ctx)
	}
}

func (l *Link) runLostHooks() {
	l.mu.Lock()
	hooks := slices.Clone(l.onLost)
	l.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
