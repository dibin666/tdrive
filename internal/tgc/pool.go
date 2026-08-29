package tgc

import (
	"context"
	"sync"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// Pool hands out API clients backed by several MTProto connections to the same
// datacenter.
//
// This is the main reason the Go port outruns the Rust reference rather than
// merely matching it. The reference streams a download through one connection
// (grammers iter_download over the client's single link), so its throughput is
// capped by one TCP stream's round-trip behaviour. Telegram happily serves many
// concurrent connections per session, and the parallel chunk reader in
// internal/reader needs somewhere to put that concurrency.
type Pool interface {
	// Client returns an API client for a datacenter.
	Client(ctx context.Context, dc int) *tg.Client
	// Default returns a client for the session's home datacenter.
	Default(ctx context.Context) *tg.Client
	Close() error
}

type pool struct {
	client      *telegram.Client
	size        int64
	middlewares []telegram.Middleware

	mu       sync.Mutex
	invokers map[int]tg.Invoker
	closers  []func() error
}

// NewPool wraps a connected client. Connections are opened lazily on first use
// per datacenter, so a drive that never touches a foreign DC never pays for one.
func NewPool(client *telegram.Client, size int64, middlewares ...telegram.Middleware) Pool {
	return &pool{
		client:      client,
		size:        size,
		middlewares: middlewares,
		invokers:    make(map[int]tg.Invoker),
	}
}

func (p *pool) Default(ctx context.Context) *tg.Client {
	return p.Client(ctx, p.homeDC())
}

func (p *pool) Client(ctx context.Context, dc int) *tg.Client {
	return tg.NewClient(p.invoker(ctx, dc))
}

func (p *pool) homeDC() int { return p.client.Config().ThisDC }

func (p *pool) invoker(ctx context.Context, dc int) tg.Invoker {
	p.mu.Lock()
	defer p.mu.Unlock()

	if inv, ok := p.invokers[dc]; ok {
		return inv
	}

	var (
		invoker telegram.CloseInvoker
		err     error
	)
	if dc == p.homeDC() {
		invoker, err = p.client.Pool(p.size)
	} else {
		invoker, err = p.client.DC(ctx, dc, p.size)
	}
	if err != nil {
		// Falling back to the single shared connection keeps the drive
		// working at reduced throughput instead of failing the request.
		return p.client
	}

	p.closers = append(p.closers, invoker.Close)
	chained := chain(invoker, p.middlewares...)
	p.invokers[dc] = chained
	return chained
}

func (p *pool) Close() error {
	p.mu.Lock()
	closers := p.closers
	p.closers = nil
	p.invokers = make(map[int]tg.Invoker)
	p.mu.Unlock()

	var firstErr error
	for _, c := range closers {
		if err := c(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// chain wraps an invoker so the outermost middleware runs first, matching the
// order the caller listed them in.
func chain(invoker tg.Invoker, middlewares ...telegram.Middleware) tg.Invoker {
	for i := len(middlewares) - 1; i >= 0; i-- {
		invoker = middlewares[i].Handle(invoker)
	}
	return invoker
}
