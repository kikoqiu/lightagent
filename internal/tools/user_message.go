package tools

import "context"

// UserMessageSignal is the channel a tool watches to learn that a user message
// arrived while the call it is running was still in flight. It is closed by
// whoever runs the call (the agent, see WithUserMessageSignal); the tool only
// receives it.
type UserMessageSignal <-chan struct{}

type userMessageSignalKey struct{}

// WithUserMessageSignal attaches a user-message signal to ctx. A tool that
// would otherwise hold the caller for a long time (run_script waiting out a
// large wait_timeout) watches the channel and cuts that wait short when it
// fires: the user message must not sit in the queue behind it. The tool answers
// with what it has — for run_script, the console output so far and the session
// the program keeps running in — and the turn carries on, so the message is
// folded into the conversation by the round that follows.
//
// A nil signal (or a nil ctx) leaves ctx unchanged: the tool then runs its work
// to the end, which is what a caller that runs tools outside a conversation
// wants.
func WithUserMessageSignal(ctx context.Context, signal UserMessageSignal) context.Context {
	if ctx == nil || signal == nil {
		return ctx
	}
	return context.WithValue(ctx, userMessageSignalKey{}, signal)
}

// UserMessageSignalFrom returns the signal attached to ctx, or nil when the
// caller attached none.
func UserMessageSignalFrom(ctx context.Context) UserMessageSignal {
	if ctx == nil {
		return nil
	}
	signal, _ := ctx.Value(userMessageSignalKey{}).(UserMessageSignal)
	return signal
}
