package tgc

import (
	"context"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
)

// The floodwait waiter is good at making one account survive a FLOOD_WAIT: it
// sleeps and retries, and the caller never sees the error. That is exactly the
// wrong behaviour to schedule on, because it hides the one fact worth acting
// on — this account has hit its limit and another one has not.
//
// So the raw error is observed on the way past. The waiter still absorbs it for
// the request in flight; this only records a deadline the scheduler reads when
// it decides where to put the next transfer.

// healthMiddleware records FLOOD_WAIT deadlines against the account. It must be
// installed inside the floodwait waiter, otherwise the waiter has already
// swallowed the error by the time it is reached.
func (m *Manager) healthMiddleware() telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			err := next.Invoke(ctx, input, output)
			if err == nil {
				return nil
			}
			if wait, ok := tgerr.AsFloodWait(err); ok {
				m.markFlood(wait)
				m.log.Warn("telegram rate limited this account; new transfers will go elsewhere",
					zap.Duration("wait", wait))
			}
			return err
		}
	})
}
