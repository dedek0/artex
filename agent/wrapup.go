package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up / settlement prompt: when an agent is terminated because [the step budget was
// exhausted (MaxTurns)] or [a timeout occurred (run_seconds/MaxDuration)], the SDK's settlement
// phase injects this prompt so the agent first persists identified-but-unwritten content and then
// outputs a one-line summary, avoiding a messy ending.
//
// Each agent's wrap-up prompt can be overridden in the admin UI as needed (stored in
// agents.wrapup_prompt); if left empty, the built-in default here is used. Only the [prompt body]
// is editable; which tools are disabled and the wrap-up's own turn budget are fixed code policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, keyed by agent key. The worker reuses the historically
// hardcoded settleWrapUpPrompt (defined in worker.go); planner/mainagent each have their own;
// unmatched (custom) agents use the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: built-in default turn budget for each agent's wrap-up phase [itself]
// (can be overridden to >0 in the admin UI). All get 10 turns to ensure enough steps to persist.
// Unmatched agents use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planning steps for this round are about to run out — note that only [this round] is ending; the system will wake you again as the situation changes to continue planning. The task is not terminating, and you do not need to wrap up the entire plan here. Land the conclusions you have already thought through and do not let this round be wasted, but also [do not force intents just for the sake of wrap-up] (0 intents this round is still a perfectly normal result): (1) if you have identified an exploration direction that [should be dispatched now], batch-submit it with one add_intent (do not hold back what you have figured out); (2) for any goal proven met by a finding/fact, call prove_goal to mark it met (do not miss any); (3) if you identified a serial exploit chain that needs steps, record it with TodoWrite so the next wake-up can continue dispatching. When done, just end this round; no summary text is needed."

const mainAgentWrapUpDefault = "Your steps are about to run out and this interaction is ending. Do not start any new exploration/operations. Please summarize the current progress, key conclusions, and suggested next steps to the user **in a single line of plain text**."

const genericWrapUpDefault = "You are about to be terminated because your budget is exhausted. First write back results that are complete but not yet persisted, then **finish with a single one-line plain-text sentence** summarizing what you did and the key conclusions you reached (this line is shown as this run's result)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up prompts (see docs/task-level timeout and wrap-up design) ----------
//
// These are a [separate set] from the per-run wrap-up prompts: per-run means "your budget for this
// run is used up"; task timeout means "the whole task has reached its deadline and is about to end".
// Their meanings are often opposite (especially for the planner: per-run says "don't stop, keep
// planning", task timeout says "stop planning at the deadline and make the final judgment").
// Configured only for worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB overrides for the task-timeout wrap-up prompt
// and turn count (wired to agents.task_timeout_wrapup_prompt / _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (this is not your run's budget — the whole exploration has reached its deadline). This is your last chance: (1) persist [everything] you have identified but not yet written back — new assets with insert_assets, exploration conclusions/facts with record_fact, confirmed findings with report_finding; (2) do not start any new commands/probes; (3) **finish with a single one-line plain-text sentence** summarizing your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (not this round — the whole task is terminating). Based on [all] current facts and findings, make one final goal judgment: for any goal proven met by evidence, call prove_goal to mark it met (do not miss any). **Do not generate any new intents** (intents dispatched now will not be executed). Wrap up once the judgment is done; no summary text is needed."

// TaskTimeoutWrapupDefault returns the built-in default task-timeout wrap-up prompt for an agent key (used as an admin placeholder / restore-default value).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // unconfigured (mainagent/chat) returns an empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. An empty string means
// this agent has no task-timeout prompt (not worker/planner), in which case the caller should fall
// back to the per-run prompt.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // by default, reuse the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is clamped by the task deadline: wrap-up due to Timeout = task
//     reached deadline → task-timeout prompt; wrap-up due to MaxTurns = steps exhausted first
//     within the clamped window, task still has minutes left → fall back to the per-run prompt.
//   - clamped=false → the task is still early: both reasons use the per-run prompt (i.e. degrades
//     to wrapupSettlement).
//
// PromptByReason, handed to the harness, picks by the [actual] reason at wrap-up time; without
// this build it would mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // task reached its deadline
				harness.ReasonMaxTurns: perRun, // steps exhausted first, task still has time
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
