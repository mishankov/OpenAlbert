// Package browser owns independent, ephemeral Chromium processes in the OpenAlbert container.
package browser

import (
	"context"
	"errors"
	"log/slog"
	"openalbert/apps/backend/internal/telemetry"
	"sync"
	"time"

	"openalbert/apps/backend/internal/domain"
)

type driver interface {
	Execute(context.Context, domain.BrowserRequest) (domain.BrowserResult, error)
	Close()
}
type factory func(context.Context) (driver, error)
type entry struct {
	createdAt time.Time
	owner     domain.BrowserOwner
	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	gate      chan struct{}
	done      chan struct{}
	driver    driver
	err       error
	closing   bool
}
type Manager struct {
	validate        func(context.Context, string) error
	logger          *slog.Logger
	mu              sync.Mutex
	entries         map[string]*entry
	limit, reserved int
	start           factory
	ctx             context.Context
	cancel          context.CancelFunc
	closed          bool
	proxy           *publicProxy
	timeout         time.Duration
}

func newManager(parent context.Context, limit, reserved int, start factory) (*Manager, error) {
	if limit < 2 || reserved < 1 || reserved >= limit {
		return nil, errors.New("invalid browser capacity")
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{entries: map[string]*entry{}, limit: limit, reserved: reserved, start: start, ctx: ctx, cancel: cancel, timeout: 45 * time.Second}, nil
}
func (m *Manager) acquire(ctx context.Context, o domain.BrowserOwner, create bool) (*entry, bool, error) {
	if o.SessionID == "" || o.ConversationID == "" {
		return nil, false, errors.New("browser owner is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, errors.New("browser manager is closed")
	}
	if e := m.entries[o.SessionID]; e != nil {
		if e.owner != o {
			m.mu.Unlock()
			return nil, false, errors.New("browser owner mismatch")
		}
		if e.closing {
			m.mu.Unlock()
			return nil, false, domain.ErrBrowserCapacity
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-e.ready:
		}
		return e, false, e.err
	}
	if !create {
		m.mu.Unlock()
		return nil, false, errors.New("no active browser; old page IDs expired, use browser_open")
	}
	background := 0
	for _, e := range m.entries {
		if e.owner.Background {
			background++
		}
	}
	if len(m.entries) >= m.limit || (o.Background && background >= m.limit-m.reserved) {
		m.mu.Unlock()
		return nil, false, domain.ErrBrowserCapacity
	}
	root, cancel := context.WithCancel(m.ctx)
	e := &entry{createdAt: time.Now(), owner: o, ctx: root, cancel: cancel, ready: make(chan struct{}), gate: make(chan struct{}, 1), done: make(chan struct{})}
	m.entries[o.SessionID] = e
	m.mu.Unlock()
	stop := context.AfterFunc(ctx, cancel)
	var event *telemetry.Event
	if m.logger != nil {
		event = telemetry.Start(ctx, m.logger, "browser.start")
		event.Set("session_id", o.SessionID)
		event.Set("conversation_id", o.ConversationID)
		event.Set("background", o.Background)
	}
	d, err := m.start(root)
	if event != nil {
		event.End(err)
	}
	stop()
	m.mu.Lock()
	e.driver = d
	e.err = err
	close(e.ready)
	m.mu.Unlock()
	if err != nil {
		m.closeEntry(e)
		return nil, true, err
	}
	return e, true, nil
}
func (m *Manager) Execute(ctx context.Context, o domain.BrowserOwner, req domain.BrowserRequest, before func() error) (domain.BrowserResult, error) {
	if o.SessionID == "" || o.ConversationID == "" {
		return domain.BrowserResult{}, errors.New("browser owner is required")
	}
	m.mu.Lock()
	closed := m.closed
	existing := m.entries[o.SessionID]
	m.mu.Unlock()
	if closed {
		return domain.BrowserResult{}, errors.New("browser manager is closed")
	}
	// Anonymous HTTP downloads and listing an empty session need no Chromium.
	if req.Action == "download" {
		return downloadPublic(ctx, req)
	}
	if req.Action == "tabs" && existing == nil {
		return domain.BrowserResult{Pages: []domain.BrowserPage{}, Text: "No browser is open for this session."}, nil
	}
	if existing == nil && req.Action != "open" && req.Action != "close" {
		return domain.BrowserResult{}, errors.New("no active browser; old page IDs expired, use browser_open")
	}
	if req.Action == "close" {
		m.mu.Lock()
		e := m.entries[o.SessionID]
		m.mu.Unlock()
		if e != nil && e.owner != o {
			return domain.BrowserResult{}, errors.New("browser owner mismatch")
		}
		if before != nil {
			if err := before(); err != nil {
				return domain.BrowserResult{}, err
			}
		}
		m.CloseSession(o.SessionID)
		return domain.BrowserResult{Text: "Browser closed. Old page IDs have expired."}, nil
	}
	if req.Action == "open" && m.validate != nil {
		if err := m.validate(ctx, req.URL); err != nil {
			return domain.BrowserResult{}, err
		}
	}
	e, fresh, err := m.acquire(ctx, o, req.Action == "open")
	if err != nil {
		return domain.BrowserResult{}, err
	}
	select {
	case e.gate <- struct{}{}:
	case <-ctx.Done():
		return domain.BrowserResult{}, ctx.Err()
	case <-e.ctx.Done():
		return domain.BrowserResult{}, errors.New("browser closed; open a new page")
	}
	defer func() { <-e.gate }()
	if e.ctx.Err() != nil {
		return domain.BrowserResult{}, errors.New("browser closed; old page IDs have expired")
	}
	if before != nil {
		if err := before(); err != nil {
			return domain.BrowserResult{}, err
		}
	}
	operation, cancel := context.WithTimeout(e.ctx, m.timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	out, err := e.driver.Execute(operation, req)
	out.Fresh = fresh
	if live, ok := e.driver.(interface{ Alive() bool }); err != nil && ok && !live.Alive() {
		m.closeEntry(e)
		return out, errors.New("Chromium stopped; old page IDs expired, use browser_open")
	}
	if operation.Err() != nil {
		m.closeEntry(e)
		return out, errors.New("browser operation cancelled or timed out; browser closed, previous action outcome may be unknown")
	}
	return out, err
}
func (m *Manager) closeEntry(e *entry) {
	m.mu.Lock()
	if e.closing {
		done := e.done
		m.mu.Unlock()
		<-done
		return
	}
	e.closing = true
	m.mu.Unlock()
	var event *telemetry.Event
	if m.logger != nil {
		event = telemetry.Start(context.Background(), m.logger, "browser.close")
		event.Set("session_id", e.owner.SessionID)
		event.Set("conversation_id", e.owner.ConversationID)
	}
	e.cancel()
	<-e.ready
	if e.driver != nil {
		e.driver.Close()
	}
	m.mu.Lock()
	if m.entries[e.owner.SessionID] == e {
		delete(m.entries, e.owner.SessionID)
	}
	close(e.done)
	m.mu.Unlock()
	if event != nil {
		event.End(nil)
	}
}
func (m *Manager) CloseSession(id string) {
	m.mu.Lock()
	e := m.entries[id]
	m.mu.Unlock()
	if e != nil {
		m.closeEntry(e)
	}
}
func (m *Manager) Reconcile(active map[string]bool, observedAt time.Time) {
	m.mu.Lock()
	var stale []*entry
	for id, e := range m.entries {
		if !active[id] && !e.createdAt.After(observedAt) {
			stale = append(stale, e)
			continue
		}
		// A crashed idle Chromium must not hold a slot until the conversation ends.
		select {
		case <-e.ready:
			if !e.closing && e.driver != nil {
				if live, ok := e.driver.(interface{ Alive() bool }); ok && !live.Alive() {
					stale = append(stale, e)
				}
			}
		default:
		}
	}
	m.mu.Unlock()
	for _, e := range stale {
		m.closeEntry(e)
	}
}
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	all := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		all = append(all, e)
	}
	m.mu.Unlock()
	m.cancel()
	for _, e := range all {
		m.closeEntry(e)
	}
	if m.proxy != nil {
		m.proxy.Close()
	}
	return nil
}

func (m *Manager) CloseTask(id string) {
	m.mu.Lock()
	var entries []*entry
	for _, e := range m.entries {
		if e.owner.TaskID == id && id != "" {
			entries = append(entries, e)
		}
	}
	m.mu.Unlock()
	for _, e := range entries {
		m.closeEntry(e)
	}
}
