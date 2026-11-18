package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side resolution of the retry policy, see docs/LLM retry design.md. Of the five layers:
//   - connection setup / empty response / same-provider safety window follow the endpoint; each LLM profile can
//     override the global default (an empty field on the profile inherits the global, and if that is unset too, the built-in default applies);
//   - circuit breaking / intent replay are process-wide, with a single global copy.
//
// The global policy is read from one DB settings row; all call sites are on low-frequency paths (building a provider,
// work wrap-up, saving config), so another cache layer is not worth it. Circuit-breaker parameters are the exception —
// they are read on every failure path, so applyRetryPolicy pushes them into the Registry for storage.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// Attempt counts keep their original semantics here: 0 = default / negative = disabled. The SDK's MaxRetries /
		// EmptyResponseRetries are fully isomorphic, so just let the SDK parse them.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// Default values for circuit breaking (round-robin cooldown), consistent with llmpool's built-ins — overridden here
// only when the user has configured a value. Defaults for intent replay are in engine.go's modelErrorRetries /
// modelErrorRetryBackoff.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob — the "empty response
// retry count": the two are two means to the same end. The SDK layer handles "not a single content block",
// by resending the same request verbatim; this layer handles "thinking only, neither text nor tools",
// by appending an instruction so the model continues with its existing thought (a verbatim resend is pointless
// for this kind of spinning determined by the shape of the context). The definitions of "empty" differ because
// the SDK goes by "has any event been yielded", and a thinking delta is itself an event — but when a user configures
// "empty response retry count" they mean "if the model produced no substantive content, try again"; sharing one
// count across both layers is what matches that mental model.
//
// Read the global policy rather than a profile override: a run may switch profiles mid-flight due to failover, and
// this is the total budget for the whole intent, which should not change with the endpoint. Semantics are isomorphic to
// the SDK's emptyRetries(): 0 = default defaultEmptyTurnNudges; -1 (negative) = disable empty-turn continuation; >0 = use the value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
