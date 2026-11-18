package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "User paused the task",
		"The user paused the task through the task-control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; running intents return to the frontier (open) and will be re-claimed and executed from the start after the task is resumed")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestrator agent paused the task",
		"The orchestrator agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; running intents return to the frontier (open) and will be re-executed after resume")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier has cancelled this task's running Planner, Worker, and main agent; this run's result will not be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored the task's paused state",
		"On startup, the backend restored the task's paused state from the persisted state in the database. This run was cancelled; normally no agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "Planner judged the task goal met",
		"The planner judged the task goal met and set the task to done, then cancelled the still-running workers; those intents are marked stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task-timeout wrap-up drain time expired",
		"After the task reached its timeout, it waited for running workers to wrap up gracefully, but the 90-second drain grace period was still insufficient, so a hard cancel was performed; intents are marked exhausted, and facts/assets already written during wrap-up are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner killed this intent",
		"The planner called kill_work to actively terminate this intent, usually meaning the direction drifted or has no further value; the intent is marked stopped and will not be re-claimed automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this Worker intent",
		"The user paused a running worker. This call was cancelled and the intent moved to paused; already-registered intents, facts, findings, and activity records are all kept, and execution restarts from the beginning after resume")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this Worker intent",
		"The user deleted a running worker. This call was cancelled; after the worker exits the write section, the server handles the intent per the user's chosen delete mode — a soft delete only marks it deleted and keeps all output, while a hard delete cascades to remove the intent and downstream nodes supported solely by it")
	AbortWorkFinished = cause("work_finished", "Worker finished and released context",
		"The worker finished normally and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, a race occurred between cancellation and the teardown event")
	AbortPausedRaceGuard = cause("paused_race_guard", "New runs refused while paused",
		"While the task is paused, the engine refuses to issue new execution contexts, preventing a race between claim and pause from letting a worker start; already-claimed intents return to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this chat turn",
		"The user clicked stop, actively aborting this main-agent or chat-agent turn. Already-produced activity records are kept, and the next message can be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause aborted the main agent chat",
		"When the user paused the task, the running main-agent chat was cancelled in sync. Already-produced activity records are kept; this turn's message is not replayed automatically after resume")
	AbortChatTurnFinished = cause("chat_turn_finished", "Chat turn finished and released context",
		"This chat turn finished normally and the server is releasing its context resources. This is not a run interruption; if it appears in an interruption message, a race occurred between cancellation and the teardown event")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running agents are cancelled; after restart, leftover running intents are reset to open and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "Per-run hard-timeout backstop triggered",
		"A run exceeded the soft wall-clock budget plus extra grace, meaning a model request or some tool did not return for a long time, so normal turn-boundary wrap-up could not execute. Focus on the last tool call that had not returned before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context reached its deadline",
			"The upstream context reached its deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceller did not attach a named cause",
			"The upstream context was cancelled, but the canceller did not attach a named cause via context.WithCancelCause; register a cause in agent/cancelcause.go and wire it into this cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
