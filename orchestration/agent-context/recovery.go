package main

import (
	"context"
	"sync/atomic"

	"github.com/kart-io/goagent/builder"
	shared "github.com/kart-io/goagent/constants"
)

// retryMalformedGeminiOnce is deliberately narrower than a general protocol
// retry. This native finish means no valid tool call was produced; Builder has
// not dispatched tools for that failed turn. Retry the same model request once
// per child invocation, retaining its failed usage and reserving a new attempt.
// A second failure, emitted content, cancellation, other providers/finish reasons
// and host/tool validation failures remain errors. No messages or clients change.
func retryMalformedGeminiOnce(provider shared.Provider) builder.RequestErrorHook {
	var used atomic.Bool
	return func(ctx context.Context, f *builder.RequestFailure) (builder.RequestErrorDecision, error) {
		if provider != shared.ProviderGemini || f == nil || f.Err == nil || f.Emitted || f.Attempt != 0 || ctx.Err() != nil || f.Err.Error() != `gemini: incomplete or blocked response (finish="MALFORMED_FUNCTION_CALL")` {
			return builder.RequestErrorDecision{}, nil
		}
		if !used.CompareAndSwap(false, true) {
			return builder.RequestErrorDecision{}, nil
		}
		return builder.RequestErrorDecision{Action: builder.RequestRetry}, nil
	}
}
