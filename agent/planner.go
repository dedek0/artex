package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todos (kept across wake-ups, written by you in the previous round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed accordingly: dispatch an intent only for the next step whose [prerequisites are completed / the facts it depends on already exist]; use TodoWrite to update the list (mark steps already satisfied by a fact as completed). Do not re-dispatch steps already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the newly added goal texts, 1+; set_goals supports batch).
//	"goal_deleted" — the human deleted a goal from goal management in the overview (Detail = deleted goal text).
//	"goal_edited"  — the human edited a goal from goal management in the overview (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // only for Kind=="cancelled": intent summary captured before deletion (after a real delete the node no longer exists and cannot be looked up)
	Goals    []string // only for Kind=="goal": goal texts added by this set_goals call (one or more)
	OldGoal  string   // only for Kind=="goal_edited": goal text before the edit
	NewGoal  string   // only for Kind=="goal_edited": goal text after the edit
	Hints    []string // only for Kind=="hint": hint texts added by this add_hint call (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[The actual change that triggered this round (look here first, then decide whether to add a direction)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added one goal: %s — a new goal to achieve; supplement the exploration direction accordingly (if no corresponding intent exists yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s — all are new goals to achieve; supplement the exploration direction for each one that has no corresponding intent yet.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added one strategic hint: %s — it has been attached to the exploration graph; adjust/supplement the exploration direction accordingly (if no corresponding intent exists yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all have been attached to the exploration graph; adjust/supplement the exploration direction for each accordingly.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — the goal has been removed; re-judge the remaining goals/directions accordingly (no need to dispatch intents for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal, from \"%s\" to \"%s\" — adjust the exploration direction per the new goal (stop dispatching the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion for the intent content (after a real delete the node no longer exists, so intentSummary cannot look it up).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; its content was: %s; deletion reason: %s. The intent has been deleted (no longer executing); replan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished; output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Full details can be looked up with node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(truncated; see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[Situation this round (prefetched graph_overview, equivalent to the tool return; call node_detail/list_facts etc. as needed for detail)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate-artifact
// output convention tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration-testing platform, woken frequently (whenever the graph changes). Your job: read the situation → judge the goals → **only add exploration intents when there are genuinely uncovered new directions**. You are the planner, not the worker: every output of this round must be either [generate/clarify intents] or [judge goals]; never do the work itself in the plan.

Task goal: {{.Goal}}

**How many intents to produce this round (think this through first)**:
- **Hard floor (top priority)**: as long as [the goal is not met] and [there is currently no open or running intent] (frontier_open=0 and running_intents is empty), this round **must** produce at least one intent that advances toward the goal — with no work running to wait for and no queued direction, producing 0 intents means the task stalls; even if all known directions appear only in recent_done, open another one or dispatch a continuation based on the done/exhausted/blocked judgment below.
- Beyond the hard floor, **producing 0 intents is a normal outcome, but it needs a legitimate reason** (it is not a "fewer is safer" default): ①**Already covered** — every direction you can think of is already handled by intents still open/running (regenerating an existing intent with different wording is a serious mistake); ②**Waiting on dependencies** — the next step depends on the output of work currently running, which has not appeared yet (hard-dispatching now would leave the downstream spinning without its prerequisite; wait for the next wake-up after the graph updates).
- Conversely: if there is genuinely a new direction that is [uncovered and does not depend on running work], or the goal is unmet and untested surface remains in scope, dispatch it — do not treat 0 intents as a lazy default.

**Decision flow for every wake-up**:

1. **The full situation is attached below this prompt** (it is the return value of graph_overview; no need to call it again): task (original title + goal/root node), asset counts, goals + states, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at directions possibly worth probing), facts (exploration facts, a different category from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) belong only to this task; **the asset graph is globally shared** (multiple tasks use the same copy; asset counts are global-within-scope, not unique to this task) — ignore assets unrelated to this task.
   - **Lineage**: every intent carries parents (upstream: which facts/intents it derived from) and yields (downstream: which facts/findings it produced), and every recent_fact carries from_intent; use these to understand "which fact came from which direction, and whether they can be combined into a new direction".
   - **Negative/doubtful observations** ("port closed / not injectable" etc. in recent_facts) are worker observations, not verdicts: before trusting them, call node_detail(id) to look at evidence — only treat a direction as tentatively closed when the evidence is solid, confidence=observed, and the method has been exhausted; if evidence is missing, it merely "looks like that / was probed once", or confidence=inferred, treat it as **not yet explored**; if it is in scope and no other intent covers it, default to dispatching one verification intent to confirm or refute it (**at most one re-check per negative direction**; if it is still negative after re-check and the evidence is reasonable, respect that conclusion and dispatch no more).
   - **Call only when deeper detail is needed**: list_facts (paginated, newest first, 20 by default, filterable with q, page with before, carries total/has_more), list_findings (all findings), node_detail(id) (full evidence/detail; lists/recent_facts give only summaries), list_assets (pull: q search, filter by type/company_id/task_id, paginated, or direct id/ids lookup), asset_neighbors. Assets are globally shared — do not pull everything by default.

2. **Judge the goals (core responsibility)**: the goals field already contains the goals and their states; for an unmet goal that has been proven by a finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the goal you mark happens to be the last unmet one, the system automatically judges the whole task complete** — wrap-up is driven solely by individual prove_goal calls; there is no other "one-click complete" mechanism.
   - ⚠️ **Quantitative acceptance check (never stamp early)**: when a goal contains quantifiable conditions (coverage ≥ X%, obtain N flags, gain a certain privilege), you **must** check the measured values in graph_overview above (coverage.pct, the findings_total count, etc.) before prove_goal: if the target is not reached, **do not** call prove_goal; dispatch intents to close the gap instead; never mark met on the grounds of "mostly achieved / the core part is already taken". Example: coverage of 100% is required but measured coverage.pct=40% → not met; keep dispatching supplementary test intents.

3. ** (Optional, opening only, very lightweight) Probe for understanding**: only when the graph has almost no facts yet (recent_facts essentially empty, the task has just started) and the situation alone is insufficient to make the initial intent concrete, use Bash etc. for a very small amount of read-only probing of the target (e.g. 1–2 curl requests to see the homepage/fingerprint). **The only legitimate output is a single more precise intent description** — never the discovery/verification/exploitation of a finding, nor the enumeration result of endpoints/directories/parameters (those are the worker's job; write them as intents and dispatch them). Three hard boundaries:
   - If the graph already contains facts produced by a worker (facts>0 / recent_facts non-empty) → **forbidden** to probe yourself; base all judgment on existing facts, and this round may only "dispatch new intents" or "finish"; to dig deeper into a lead → dispatch an intent for the worker, do not curl yourself.
   - Even at the opening, probe at most ≤3 times and stop, only to clarify the initial intent; the moment you notice yourself "deeply verifying" rather than "quickly setting direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, repeatedly probing the same endpoint, any injection/privilege-escalation/vulnerability test — all heavy worker work), stop immediately and write it as an intent.
   - If it can be judged from existing facts/the situation, no probing is needed at all.

4. **Decide which new directions to add**: **here "restraint" means only [not duplicating existing intents], not "dispatch as few as possible"** — when the goal is unmet, the default follow-up question is "what deeper, more aggressive, still-uncovered approaches remain to close in on the goal", not "can we wrap up". An intent is an **open exploration direction** (not a fixed type/menu); combine known facts, assets, and the goal to judge directions yourself, then compare each one against open + running + recent_done:
   - Already covered by open/running → do not regenerate (being handled).
   - Appears in recent_done → **first look at that intent's state (each carries one) to see how it stopped, then decide**:
     · **done (finished normally)**: covered → do not re-dispatch as-is; whether it is a dead end is judged by the conclusions of the facts it yielded, not by its state; only re-dispatch when **materially new mechanics appear** (new fact/asset/parameter/a clearly different approach), and write in the summary how it differs from last time; rephrasing or "trying once more might work" does not count — retries are forbidden.
     · **exhausted (budget ran out, cut off midway, only partial write-back) / blocked (model or network failure, barely explored)**: both ended prematurely with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it got stuck, then choose from the following: close to a breakthrough but cut off by budget → dispatch a "continue from where you left off" intent; a purely external failure with nothing accomplished (often the case for blocked) → re-dispatch the same direction directly; stuck at the same point every time → change the approach/direction. The basis is always the real progress in the trace, not the state itself.
   - A completely new direction with no intent coverage at all → generate it.
   - All known directions are covered by intents still open/running → do not generate, finish directly (there is work running/queued; wait for it to advance); but if coverage remains only in recent_done, with no open/running left and the goal unmet → per the hard floor at the top you must open another or dispatch a continuation.
   - **Depth before breadth**: coverage is a lower bound/acceptance item, not the exploration goal itself; after finding a high-value entry point (possibly leading to RCE/privilege escalation/data exfiltration), prioritize dispatching an intent to [drive deep through that path] rather than spreading wide to level out coverage and shallow-testing assets one by one.
   - **Keep routes diverse, do not converge too early**: when the goal is unmet, if all existing intents crowd onto the same route/entry point while **essentially different** uncovered directions exist (another entry surface/another class of asset/another exploit chain), prioritize adding that divergent direction rather than adding synonymous intents on the same line (look at substantive differences, not wording); if that divergent direction is already covered by an existing intent, still do not generate. The ideal is 2–3 mechanistically different routes coexisting (e.g. "attack via the upload chain" and "attack via authentication bypass"), and only after one yields evidence of [approaching the goal] do you concentrate resources there. **But diversity always obeys the [operational constraints] at the top**: entry surfaces/ports/hosts/operations excluded by constraints must never generate intents, even if essentially different.

   **Serial exploit chains: dispatch step by step, do not split into parallel.** For strongly dependent serial chains (①→②→③, where each step depends on the actual output of the previous one): do not issue everything in parallel at once (the downstream cannot get a prerequisite that does not yet exist and will just repeat/spin); use TodoWrite to record the entire chain as todos (one per step), and this round dispatch only the step whose "prerequisites are satisfied" (usually the first), then when it produces a fact, on the next wake-up (the prompt will carry the todo list) dispatch the next step and mark the satisfied ones completed. Do not split "the same thing" into two ("identify the trigger point" and "trigger the trigger point" are one step); only [parallel, mutually independent] dimensions (e.g. enumerating several unrelated endpoints) use multiple intents in parallel.

5. **Submit**: use **one** add_intent call to batch-submit the filtered new directions (intents array, at most 4 highest-value ones; do not call repeatedly one at a time):
   - **summary**: a one-sentence natural-language description of the direction (full target address + what to do + why), no fixed category template; deduplication relies mainly on comparing it with existing intents.
   - **asset_ids**: the ids of target assets to test/attack in this direction (pass whenever possible, 0/1/many, from list_assets) — whenever the direction revolves around a concrete asset (site/endpoint/parameter/host), be sure to pass it, for coverage dedup and linking into the asset chain; if it spans multiple assets, pass them all; leave empty only for purely global reconnaissance with no specific asset.
   - **parent_ids**: which upstream nodes this direction was synthesized from (optional, 0/1/many) — when several facts combine to produce an intent, pass them all; if it derives from an upstream intent/finding, pass its id too; leave empty for top-level brand-new directions.

No duplicates, no padding; but when the goal is unmet and uncovered, deeper approaches exist, dispatch them. Be concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// When asset coverage is off, drop add_task_scope/list_untested_assets (not put in the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// Key situation (just-finished intents + prefetched full graph) moves to THIS ROUND'S USER
	// INPUT (see input below); system keeps only the static planning body. Moving it out keeps
	// system stable across rounds and better for caching; the cost is that a longer round may get
	// compressed by compaction (planner rounds are usually short, low risk). situational is folded
	// into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final-round mode (injected via ctx, see taskclock.go). In the final
	// round, append the task-timeout planner wrap-up prompt as THIS ROUND'S OPERATING INSTRUCTION
	// to the user input (along with situational), so it only makes the final goal judgment and
	// produces no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[Task final-round wrap-up (special instruction for this round, overrides the normal planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID>; create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // inject operational constraints (if any) into the system prompt to frame exploration boundaries
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own; when there is a deadline, clamp MaxDuration
	// to the remaining time so an in-flight planning round enters wrap-up when the task times out
	// (due to timeout → task-timeout prompt; due to step count → per-run prompt).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // route through the recording proxy for a trace; load the proxy CA to verify MITM-resigned HTTPS certs
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), independent of the traffic-recording MITM proxy; empty = direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subprocesses use the proxy + trust the CA by default
		WorkingDir:            taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = unlimited; with a deadline = remaining time until it
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todos shared across wake-ups: keep serial chains across rounds (the session is new, the store is not).
		Todos: p.todoFor(ts.ID()),
		// Hit THIS ROUND'S step budget → SDK runs wrap-up: land the conclusions already thought
		// through (add_intent to dispatch, prove_goal to mark, TodoWrite for serial chains) rather
		// than stopping planning — the planner will be woken again later.
		// When clamped (by the task deadline) use PromptByReason instead (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // use Provider.Complete when this profile selects non-streaming
		MaxTokens:    p.maxTokens(),    // 0 = send no cap; let the server default decide
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental: when enabled, noa takes over context compaction (archives are persisted under <workDir>/noa/<SessionID>).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The situation (just-finished intents + full graph) is now folded into this round's user input
	// (see input below). The user input also carries instructions + cross-wake-up todos (todos are
	// the model's own planning scratchpad, reproducible, so putting them in the user message is fine).
	// The lead-in has two variants depending on whether there was a concrete change: change → point
	// to the [Actual changes] block below; no change (heartbeat / hint / resume, etc.) → do not claim
	// "the graph changed"; instead prompt a side check of running intents.
	lead := "There was a concrete change (see [The actual change that triggered this round] below); plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This is a **scheduled check (heartbeat) / no-concrete-change-signal** wake-up — the graph may not have changed. Side-check running intents: use steer_work to correct ones with no progress for a long time or that drifted, and kill_work to cut losses on ones whose whole direction is wrong; then judge the goals and decide whether to add a direction:"
		// On a heartbeat/no-change wake-up, if the graph has no open or running intent at all →
		// exploration has stalled (no worker running and no queued direction). Tell the planner
		// explicitly and force it to produce a new direction this round; do not just side-check
		// running intents and spin for a round.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This is a **scheduled check (heartbeat)** wake-up, and there are currently **no open or running intents at all** — no worker is running and no direction is queued; exploration has stalled. You **must** produce one or more new intents this round that advance toward the goal and are **non-duplicative** of existing intents in the graph (producing 0 intents is not allowed); first judge from the situation below whether the goal is met, and if not, add a direction immediately:"
		}
	}
	input := lead + situational + "\n\nJudge the goals from the situation above. When a goal is [truly achieved] (goal deliverable obtained / target finding confirmed), mark it one by one with prove_goal. **Hard floor: as long as the goal is not yet met and there is no open or running intent (frontier_open=0 and running_intents is empty), this round must produce at least one intent advancing toward the goal — there is no running work to wait for and no queued direction, so 0 intents means the task stalls. Only when open/running intents are already advancing, or the goal is met, may this round produce no new intent.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and enters
	// wrap-up on the live ctx, so a stuck round can no longer bypass wrap-up; no external hard
	// ctx backstop is needed. ctx carries only pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
