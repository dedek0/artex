package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration-test goal decomposer. Your job is to identify the **final deliverable outcome** from the user's input — not to plan attack steps.

**Step 1 (do this before splitting goals): extract operational constraints**
From the "Task goal / Task description", identify the operator's explicit rules about [what may and may not be done] and register them one by one with set_constraints (if the description/goal does not involve operational constraints, you may skip this extraction):
- type=deny: forbidden operations (e.g. "do not scan ports", "do not perform write/delete operations against production", "no brute forcing", "do not touch a certain subdomain").
- type=allow: explicitly permitted/limited scope of operations (e.g. "passive reconnaissance only", "target only a certain domain").
- A constraint ≠ a goal, and ≠ an attack step: it is a rule bounding operational behavior.
- **Constraints must be self-contained and hard-code the specific target**: replace **deictic references** such as "current target/current port/current IP/current domain/this site" with the **concrete values** from the task goal/description. Constraints are injected separately into the execution-stage prompt; out of context, a deictic reference cannot be resolved to what it refers to.
  Example: the goal is https://abc.example.net → write "only test abc.example.net", not "only test the current target"; "test only target port 443, do not scan other ports", not "test only the current port". If the original text says only "current target" but the target address is explicit, fill in the address.
- **Register only constraints that are explicitly stated or emphasized in the goal/description; never fabricate any**; when unsure of the type, use deny (the more conservative choice).
- If the goal/description truly contains no operational constraints, **do not** call set_constraints.
After registering constraints (if any), proceed with the goal decomposition below.

**Goal = a final, deliverable/verifiable outcome**

**What is NOT a goal (do not list as sub-goals)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification processes
- Attack steps, exploitation methods
- Result-verification steps

**Decomposition principles**:
- The user describes exactly one final goal → output one
- Multiple **mutually independent** final deliverables exist → list each separately
- Tag vulnclass when it maps to a clear vulnerability class; leave it empty for information-gathering/business-logic goals
- Never fabricate goals the user did not mention

Call set_goals to submit the result.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `
**Additional responsibility: register the test-asset scope**
In addition to decomposing goals, you must identify the **explicitly given test-asset scope** from the "Task goal / Task description" and register it with add_task_scope (the authorization boundary for this task, and also the denominator for asset-test coverage). **Minimal-scope principle: register only the single target the user explicitly named; never widen it on your own.**
- If the goal is a URL or an address with a host name (e.g. https://xxx.example.com/path, app.example.com) → take its **full host name**, kind=subdomain, value=full host name.
  Example: goal https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  It is **strictly forbidden** to shrink a host name that has a subdomain down to the root domain — seeing xxx.example.com and registering the whole example.com would expand the scope beyond the user's target, violating the minimal-scope principle.
- Only when the user gave a **bare root domain with no subdomain at all** (e.g. example.com written directly), or explicitly said "the whole site / all subdomains / the full domain" → use kind=root_domain, value=example.com.
- A bare IP or network segment → kind=ip / cidr, value=IP or CIDR.
- **Do not** register a company scope (company) — the task has just been created and the asset system usually does not yet have that company, so registration would fail; company-level scope is handled later in the plan phase.
Other rules:
- Register only scope **explicitly written in the goal/description**; never fabricate or infer domains/IPs that were not mentioned.
- reason briefly states which sentence the basis came from, for auditability.
- If the goal/description contains no explicit asset scope, **do not** call add_task_scope.
First register scope with add_task_scope (if any), then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (context: target scope / number of flags / engagement notes, etc.).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: no transcript store is attached, so agentcore
	// does not put a session id on ctx (it only does so when there is a writer, see
	// agentcore.Prompt). Gateways that do prompt caching / sticky routing by the session-id
	// header (opencode zen returns 400 MissingSessionID when x-opencode-session is absent)
	// read exactly that value from ctx — without it, chat works but decomposition returns 400.
	// Attach a stable id explicitly: decomposition requests for the same exploration share it
	// (good for cache hits), and the name does not conflict with planner/worker so llmrec.parseSession
	// can attribute it correctly.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (does not depend on the asset store): the body already
	// contains the "extract operational constraints before splitting goals" step (wording can be
	// changed on the agent edit page); here we only need to wire up the tool.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background information, may include target scope / number of flags / engagement notes; for reference only, do not fabricate anything not mentioned in it):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// The 3 steps (extract constraints → register scope → split goals) each need one tool call; give enough turns so set_goals is not missed before wrap-up.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // use Provider.Complete when this profile selects non-streaming
		MaxTokens:    maxTokens,    // 0 = send no cap; let the server default decide
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
