# Changelog

All important changes to this project are recorded in this file, following the [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) format.

## [Unreleased]

### Deployment and release maintenance (keeping program version 0.3.15)

- Migrated to a standalone `RuoJi6/ARTEX` repository, preserving the original commit history and branches.
- The in-page online update source is `RuoJi6/ARTEX`; GitHub Actions builds packages for five platforms and the SHA256 verification file.
- Build Docker Hub images on native AMD64 and ARM64 runners, and verify program startup, database connectivity, and the embedded frontend.
- After both architectures pass verification, publish the `v0.3.15` and `latest` images and the corresponding packages.
- Compose files and deployment instructions use this repository's images, with added guidance on migrating existing deployments while preserving data.

## [0.3.15] - 2026-10-07

### Interception

#### Added

- **Added a built-in intercept rule for "destructive endpoint paths"**: previously the built-in HTTP destructive rule only recognized the DELETE **method**, while most applications' delete endpoints can be triggered with GET/POST, so calls like `curl 'http://t/api/user/delete?id=1'` actually delete the target data. A `deny` rule has been added covering `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, and `/delta` are not falsely blocked). Existing instances receive it after upgrading; it can be disabled or removed under "System → Command Interception".
- **Token usage for model-fallback approval can now be viewed separately**: approval calls previously had no separate metering attribution, so their cost could only be guessed. They are now metered separately: a usage card has been added below the "Model Fallback Approval" toggle under "System → Command Interception", with cumulative stats for five metrics — call count, input, output, cache reads, and cache writes — plus a mini bar chart of daily usage over the past 30 days.

### Finding notifications

#### Added

- **Findings can now be pushed to IM**: added a "System → Notification Push" page that can push scan findings to six channels: **DingTalk, Feishu, WeCom, generic Webhook, Telegram, and email**. Any number of instances can be configured per channel type, each with its own enable/disable, rate limiting, and filter rules.
- **Two delivery modes: realtime and digest**: realtime sends each match immediately; digest merges them into one message on the global interval (30 minutes by default), with the count of new findings and their severity distribution at the top. To get "high severity realtime, everything else digest", create two channels configured separately.
- **Filter rules support four dimensions**: minimum severity, task/asset scope restriction, finding-type keyword inclusion and exclusion (exclusion takes precedence), and whether to receive disposition-status changes (off by default). Messages include a "View Details" button; the target URL comes from the global "Callback URL" configuration, and no button is added if it's left blank.
- **Delivery history and manual resend**: lists each delivery's status, attempt count, failure reason, and channel; filterable by channel and status. Failed items can be resent with one click (resending resets the retry counter).
- **Masked display of channel credentials**: Webhook URLs, signing secrets, bot tokens, SMTP passwords, etc. are shown only as masked values with a hint of the trailing characters. Returning the masked value unchanged on submit means "don't modify"; clearing the field deletes it.

#### Fixed

> This section is the result of a security audit performed after the initial feature was complete; each issue was reproduced, then fixed, then covered with a regression test.

- **Fixed a bypass of credential masking by "changing the target URL while keeping the credential" (Critical)**: configuration merging always preserved the stored value for "keys not mentioned", so changing only the address and saying nothing about the credentials made the server send the real stored credentials to an arbitrary address, completely silently. Now, once the target URL changes, every credential field must be stated explicitly — provide a new value, or explicitly leave it blank to indicate it's no longer needed.
- **Fixed the lack of escaping for untrusted content in markdown-based channels**: finding titles and summaries come from model output and asset URLs from scan results; a crafted title renders as a clickable external link in DingTalk / WeCom / Feishu, while image syntax makes the client fetch a URL during rendering and leaks the reader's IP. All such content is now flattened to a single line and markdown metacharacters are escaped, with escaping applied at each channel's rendering exit rather than in a shared title function.
- **Fixed the SSRF surface of delivery URLs**: previously loopback, internal-network, and cloud-metadata addresses could all be delivered to, and the first 200 bytes of a failed response body went into `last_error` and were shown in delivery history, forming a semi-blind read primitive. Defenses are now applied at the **dial stage**, covering DNS rebinding, and cross-host redirects are rejected. Loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1`; RFC1918 private networks remain allowed (self-hosted internal SMTP relays are common).
- **Fixed leakage of push credentials through error messages**: DingTalk's `access_token`, WeCom's `key`, and Telegram's bot token are all in the URL, and on delivery failure the full URL flowed into `last_error`, delivery history, server logs, and frontend notifications. These are now uniformly redacted, keeping only `scheme://host` and the underlying cause.
- **Fixed silent loss when a truncated digest message caused the whole batch to be marked delivered**: truncated findings appeared neither in the message nor in the failure list, yet delivery history showed success. Packing is now done **per message**: only the findings that fit into a given message are marked, the rest return to the queue for the next message without consuming retries, and the message header states how many more will continue in the next one.
- **Fixed a typo in the minimum-severity threshold silently disabling the filter**: when `min_severity` was misspelled as `hgih`, the unknown severity had ordinal 0 and the comparison degenerated to always-true, so users who thought they were receiving only high-severity findings actually got all of them in the group. Values are now validated on the write path, with allowed values listed.
- **Fixed `rate_per_min = 0` (no rate limit) being unreachable**: the database-write layer silently changed an explicit 0 to the default value, so operators thought they had disabled rate limiting when they were still being throttled. The default is now filled in at the API layer when the field is absent.
- **Fixed digest channels completely bypassing the token bucket**: `rate_per_min` previously had no effect in digest mode. The number of items claimed is now bounded both by the current round's allowance and by an in-memory upper limit.
- **Fixed failure handling using the batch's maximum attempt count, dragging old deliveries down with new ones**: an old delivery already retried twice would pull brand-new deliveries in the same batch into `failed`, permanently losing new findings before they'd used a single retry. Decisions are now made per item.
- **Fixed deliveries with unparseable snapshots in a digest batch being silently marked successful**: such entries were skipped during rendering yet still counted as successful with the batch. They are now explicitly marked failed with a reason.
- **Fixed the per-channel, per-round delivery count possibly exceeding the lease duration**: in multi-instance deployments, rows with expired leases were re-claimed, sent twice, and had their attempt count incremented twice. The per-round cap is now derived from "lease / single timeout", with assertions pinning down the relationship between these constants.
- **Fixed Telegram truncation potentially cutting HTML entities in half**: truncation avoided partial tags but not entity fragments like `&amp`, which could make the parser reject the **entire** message. Both are now avoided.
- **Fixed the email channel treating transient SMTP failures as permanent**: greylisting `450`-style 4xx responses are temporary rejections that should be retried later, but were previously all treated as fatal, so servers with greylisting made every push fail on the first attempt. The first digit of the response code now decides: 4xx is retryable, 5xx is permanent failure.
- **Fixed nested-structure submissions writing the mask literal into the database**: object fields such as `webhook.headers` can only be masked or submitted as a whole; putting a mask sentinel inside the object was stored as a real value, silently breaking authentication. Such submissions are now explicitly rejected.

#### Design notes

- **The finding-writing transaction does only one blind INSERT**: `notification_events` is written in the same transaction as the finding, so commit guarantees atomic consistency. This INSERT deliberately doesn't read the channel table or run the user's filter rules, and is wrapped in a `SAVEPOINT` — otherwise a single misconfigured filter condition could abort the transaction and prevent a high-severity finding from being stored.
- **Delivery uses lease-based claiming rather than long transactions**: after claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as a lease, so no database lock is held during delivery. Rows left behind by a crash are re-claimed after the lease expires — self-healing and without infinite retries.
- **Rate limiting doesn't consume the retry budget**: the token bucket first determines this round's allowance, then that many items are claimed. Reversing the order would make rate-limited deliveries burn an attempt; after three purely waiting-based attempts exhausted the budget, they'd fall into `failed`.

### Test infrastructure

#### Fixed

- **Fixed permanent asset residue caused by cleanup order in the company ICP attribution test**: `defer d.Close()` ran first when the function returned, before `t.Cleanup`, so all cleanup statements hit an already-closed connection and their errors were discarded. Test assets and companies stayed in the database forever and caused mysterious failures in other tests that assert on asset counts. Connection closing now also goes through `t.Cleanup` and is registered first, and cleanup failures are made visible.

### Traffic

#### Added

- **Traffic list supports content search, path/status-code/length filters, and column sorting**: the filter row supports filtering by **response content** (via full-text index), **path**, **status code** (exact value or buckets like `2xx`), and **response length range**; column headers sort by time, status code, and response length, and sorting preferences are stored locally. Two new indexes take effect automatically after existing instances upgrade; the Agent tool's query contract is unchanged.
- **Added "Clear All" to the traffic list**: deletes all traffic records at once (ignoring the current filters), cleans up historical host directories no longer tracked by the index, and performs a full compaction afterward, reporting how much space was actually freed. Traffic evidence already bound to findings lives in a separate evidence store and is unaffected.

#### Fixed

- **Fixed disk space not being reclaimed after deleting traffic**: the index database was created without `auto_vacuum`, so the file never shrank; the full-text index also uses `contentless_delete`, where `DELETE` only writes a tombstone without reclaiming the original postings — deleting traffic actually made the index grow (in testing, capturing 6 MB of content produced a 16 MB index that stayed at 16 MB after deleting everything). New index databases now enable `auto_vacuum=incremental`, and after each deletion full-text index merges and incremental reclamation run in background chunks, gradually returning space to the OS (the same scenario dropped to 104 KB). Reclamation releases the write lock between chunks so traffic recording isn't blocked.

> Upgrade note: `auto_vacuum` can only be set when the database is created, so **existing instances' index databases remain in the old mode** and incremental reclamation is a no-op on them (a notice is printed at startup). After upgrading, tombstone growth is halted on such databases; the already-occupied space can be reclaimed once with "Clear All" — that step converts the database to incremental reclamation mode, after which ordinary deletions take effect automatically.

### LLM

#### Fixed

- **Fixed custom session headers not taking effect on "one-shot LLM call" paths**: goal decomposition (round 0) and cold-node compaction are one-shot calls without a transcript store, so there's no session id on the context and the gateway can't read the header — on endpoints like opencode zen that return 400 when the header is missing, this shows up as "round 0 fails while later planner rounds are completely fine". These two paths now explicitly attach a session id stable per exploration, which also lets token usage be correctly attributed to the corresponding exploration (previously it wasn't recorded at all).

### Tools

#### Fixed

- **Fixed output truncation not taking effect for MCP and custom tools, flooding the context with large results**: MCP tool returns previously weren't truncated at all, while HTTP-based custom tools used a hardcoded 6000-character limit instead of the session-configured one because they received an empty tool context. Both paths now go through `actool.Capture`: truncate at the session's `MaxOutputChars` (default 30000), and when `ToolOutputDir` is configured, spill the full output to disk, keeping only the opening excerpt and a file pointer in the message.
- **Upgraded norma to v0.4.3**: the harness now runs `CaptureOnce` uniformly, so no tool (including future ones) can overflow the context by failing to wire up truncation.

### Deployment

#### Fixed

- **Fixed the activity feed failing to connect behind a reverse proxy**: the SSE URL was previously always built as `${hostname}:8787`; in deployments proxied to port 443, the page loaded from `https://<domain>/` while SSE pointed at port 8787, which isn't publicly exposed, causing the activity feed to reconnect repeatedly. Behavior now depends on `NODE_ENV`: production defaults to same-origin, and only `next dev` keeps `:8787` to bypass Next.js's SSE buffering; the `NEXT_PUBLIC_SSE_BASE` override remains. The README now includes reverse-proxy deployment instructions.

### Findings

#### Fixed

- **Fixed the "By Asset" view having no scrollbar when the asset list overflowed**: the outer card only had `max-height` and no definite height, so the percentage height of the scroll viewport couldn't be resolved; with many assets the list either burst out of the card or was cut off with no way to scroll. The height cap is now applied directly to the asset tree's native scroll container and adapts to the window height.

### Database

#### Fixed

- **Capped the connection pool**: `database/sql` doesn't limit the number of connections the application itself can open by default, creating new ones unconditionally when none are idle, until PostgreSQL's `max_connections` (100 by default) rejects them. The cap is now 32, and excess queries wait for an idle connection — under the same load this shows as slower rather than errors. If `max_connections` was lowered, lower this accordingly.
- **Improved connection reuse**: the default `MaxIdleConns` is 2, so with concurrency above 2 connections were closed right after use and the next request had to redo the TCP and PostgreSQL authentication handshake. The idle cap is now raised to match the total cap, with a 30-minute connection lifetime and 5-minute idle eviction added so the pool can naturally rotate long-lived bad connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task list

#### Added

- **Added a "Findings" column to the task list**: shows counts by severity tier — Critical / High / Medium / Low — with non-zero tiers colored by severity, making each task's finding volume and distribution clear at a glance.

#### Changed

- **Narrowed the "Description / Goal" columns**: overly long content is truncated with the full text available on hover, reducing how much horizontal space long text takes in the list.

### Exploration graph and planning overview

#### Changed

- **Streamlined the planning overview (`graph_overview`) and added caps to each list**: recent facts, finished intents, pending intents, cold-zone digests, and confirmed-finding details now all use "latest window + count fallback", with undisclosed items queryable on demand. This significantly reduces per-round LLM context size and prevents context bloat on long tasks (the cold zone of the related-task overview is rate-limited as well).

#### Fixed

- **Eliminated reverse duplicate edges between digest and finding nodes in the exploration graph after collapsing cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset interception

#### Added

- **Added global asset intercept rules (blacklist)**: supports entering exact and fuzzy matches for domains / IPs / URLs plus CIDR ranges, with create/read/update/delete and enable/disable for rules; the management page is under "System → Asset Interception". By default it includes fuzzy blocking of government (`.gov` / `.gov.cn`) and educational (`.edu` / `.edu.cn`) sites.
- **Asset interception wired into the execution chain**: before issuing an intent (`add_intent`) or inserting assets (`insert_assets`), the Agent first checks the target assets against the intercept rules — intercepted intents are not issued and intercepted assets are not inserted, with asset information and the intercept reason returned to the Agent.
- **Added task-level intercept / allow (whitelist) rules**: independent of the global rules and effective only for the given task. Evaluation order is "intercept first, then allow": a matching intercept rule forbids testing; if no intercept matches but the task has allow rules configured and none match, testing is "not allowed"; if no allow rules are configured, the whitelist isn't applied. Rules can be entered at task creation or edited, enabled, and disabled in the task detail "Overview".
- **Task templates support preset categories and task-level intercept/allow rules**: a template can store a task category and a set of task-level rules, which are loaded into the new-task form when the template is applied.

### Operation review

#### Fixed

- **Tightened the model judge output protocol to reduce false allows caused by truncation**: lowered the limit on the verdict comment from 500 Chinese characters to 120 and require "JSON only, no preamble or code fences", so a verdict truncated by `MaxTokens` can't become unparseable and then be allowed under the model-failure policy (fail-open).

### Task archiving

#### Fixed

- **Archiving now skips symlinks instead of failing the whole package**: the archive format only supports regular files and directories end to end; previously any symlink in the working directory made the entire task archive fail. Symlinks are now skipped with a log entry while other files archive normally (links aren't followed and the directory tree isn't escaped).

### Accounts and compliance

#### Added

- **Added a "Terms of Use and Disclaimer" dialog before login**: users must check the agreement box before they can log in.

### License and dependencies

#### Changed

- **The project adopts the AGPL-3.0 open-source license**, with expanded license and disclaimer information in the README.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration trace feed

#### Added

- **Added an "Exploration Trace Feed" to the task detail page** (#144): presents a task's exploration nodes (start / goal / intent / fact / finding / hint / digest) as a timeline stream, with type filters, keyword search, ascending/descending order, pagination, and auto-refresh. While on "page 1 with the newest first" it acts as a live view and refreshes every round; leaving that position only accumulates an unread count without disturbing the current reading, with a one-click "N new updates · Back to latest" button. Items are grouped by day so even when paging through a long task you can tell "which day this happened".
- **The feed shows the node id on each row** (#145), making it easy to cross-reference the exploration graph and locate a specific node.
- **Expanded feed node details show upstream/downstream links and anchored assets** (#147): expanding a feed entry reveals the node's upstream (where it came from) / downstream (what it produced) relationships; hovering a related item pops up that node's card (type / status / origin / time / summary / payload excerpt). It also lists the assets anchored to the node (type tag + readable text). The related data is delivered with the feed page, so expanding sends no extra request.
- **Feed search supports filtering by node id** (#150): besides "content / origin", the search box now matches node ids; enter a plain number or the displayed "#41" form to pinpoint a node.

### Intent management

#### Added

- **Intent deletion now supports "soft delete / hard delete" modes** (#149): pending, running, and paused intents can all be deleted, with a required reason; the mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is set to "deleted" and the reason is recorded in a separate field; the intent node and all its outputs and lineage are preserved.
  - **Hard delete**: physically removes the intent along with descendant nodes supported "only by it" (cascading to leaves along the output / intent chains), avoiding orphaned data. Token usage for the deleted intent is archived by its original date. Shared nodes (still referenced by other intents), goals, and the task's root facts are always preserved, and the hard-delete dialog shows the estimated number of affected nodes.

  Both modes notify the planner that "the intent was deleted by the user + reason" and trigger replanning.

### Operation review

#### Added

- **Approval records can be filtered by status and decision source** (#139): filter by approval status and decision source to quickly locate target records.

#### Fixed

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by history-list pagination and stay fully visible while browsing history.

### Agent

#### Changed

- **Reworded the Worker role description in generic cybersecurity-platform terms** (#138).

#### Fixed

- **Fixed cold-node compaction never triggering**: wired the Compactor into the per-task planner; cold-node compaction (cold-digest) previously never ran because it wasn't connected, and has now been restored.

### Chat

#### Added

- **Chat supports @-mentions of multiple record types with scrolling pagination** (#135): you can @-reference records of various types in chat, and mention candidates load with scrolling pagination.

#### Fixed

- **Long message bubbles are constrained within the conversation panel** (#137): overly long bubbles no longer overflow the conversation area.
- **Fixed "missing upload file" when uploading before a new conversation is created**: the composer now snapshots `FileList` into an array before clearing the input and then calls back. Previously, draft state required asynchronously creating the conversation first, and by the time execution resumed the live-bound `FileList` had been cleared, so uploads lacked the file field and the backend returned 400.

### Traffic

#### Fixed

- **The traffic-recording proxy now listens on 127.0.0.1 by default** (#129, #130): prevents an open proxy from being exposed on other network interfaces under the default configuration.

### Web

#### Fixed

- **Fixed the static-export task list page missing the global header**.
- **Added the missing related-traffic mock on the demo finding detail page, fixing a blank screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### Added

- **Approval records support source navigation** (#125): clicking an approval record's "Source" jumps back to the exact tool execution that triggered the approval — opens the original full conversation, auto-paginates to the target position, expands the command and result in place and centers the highlight in the scroll area while keeping neighboring messages readable; automatic correction stops when the user scrolls manually. Across regular conversations, task Workers, the planner, and segmented main-Agent sessions, locating works precisely via the persisted `tool_use_id` and task mapping (a scoped call-ID index was added); when the ID is missing, duplicated, or ambiguous, an explicit notice is shown instead of jumping to the wrong execution. A clear notice is also given when the conversation was deleted or the task archive/records are missing.
- **Approval records support pagination** (#115).

#### Changed

- **Streamlined model-review input** (#124, #125): the input to model review (the LLM judge after no rule matches) is now a versioned JSON consisting of "the complete current tool call + a clearly selected brief context + the local working directory". Context includes only real user messages (the current user message of the chat / task main Agent); Workers no longer attach intent summaries and inherited context from parent Agents is cleared; the planner and auto-triggered sessions don't synthesize user messages. Task description, goal, operation constraints, global exploration state, past calls, and the Worker's full intent are no longer attached — these are still used for Agent execution and separate session audits under the existing logic, just not for operation review. Every verdict is required to output JSON with `decision`/`comment`, and the comment must cover "the actual operation, the consequences if it succeeds, and the matched rule"; instructions in arguments or context can't change the review policy. The exact input sent to the model is saved as a snapshot with a fingerprint; old snapshots are retained and version-labeled, never reconstructed from current data.

#### Fixed

- **Model verdicts wrapped in code fences no longer silently allow** (#126): if the verdict JSON returned by the model was wrapped in a ``` code block, parsing previously failed and the model-failure policy silently allowed it. Code-fence markers are now stripped before parsing, and only if parsing still fails is the configured failure policy applied.

### Agent

#### Added

- **Added experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature available in system settings, off by default. When enabled, noa (model-driven context compaction) takes over context compaction for the four Agent types — main Agent, planner, Worker, and chat — replacing the built-in compactor. The original compacted text is archived to persistent storage, collected under `<workDir>/noa/<session-id>/` (not scattered across task directories), with a globally unique directory per session id. If integration fails, it automatically falls back to built-in compaction without interrupting real tasks. The toggle is read once per run, so switching it only affects runs started afterward.

#### Fixed

- **Exploration-graph tools now reject calls outside a task context instead of nil-panicking**: calling exploration-graph tools without a task context returns a clear error instead of crashing on a nil store dereference.

### MCP

#### Added

- **Support for legacy SSE MCP servers** (#117): compatible with MCP servers that only offer the older SSE transport.

### Traffic

#### Fixed

- **Traffic search matches recorded hosts in a port-aware manner** (#114): `traffic_search` now includes the port in host matching, preventing records for the same host on different ports from cross-contaminating each other.
- **An error in the `traffic_search` description migration no longer interrupts later reporter migrations**: a single migration failure is isolated and doesn't affect subsequent migrations.

### Web

#### Changed

- **Added separators between retest-finding options** (#122): dividers between retest findings make the selection clearer.

#### Fixed

- **Fixed a whole-page crash on the demo task detail page**: in mock mode the task detail "Conversations" tab fetches `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route; the read fallback treated "path ending in s means collection" and returned `[]`, making `data.items` `undefined`, so the side hook's `merge()` threw `TypeError: t is not iterable` while iterating it. That exception occurred inside a `setItems` updater and was deferred by React to the render phase, where the caller's `catch` couldn't intercept it, so the error boundary took over the whole page and showed "This page couldn't load". The mock handler now explicitly returns empty side-question history, and `sideAPI.history` also normalizes its return value (anything whose `items` isn't an array falls back to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Networking

#### Added

- **Added DeepSeek's official source to web search**: directly reuses the currently active LLM configuration. It differs in nature from the other three sources — DeepSeek has no directly callable search API; search exists only inside its Anthropic-compatible interface (the `web_search_20250305` server-side tool), executed by DeepSeek's server. Therefore it **only supports DeepSeek official endpoints + the anthropic protocol** (OpenAI-protocol endpoints reject server-side tools outright), and each search **consumes an extra model call**, requests **don't pass through the search egress proxy**, are **not recorded in traffic logs**, and results contain **only titles and links** (no summaries; WebFetch retrieves content when needed). The settings page states these limitations but **doesn't validate or block** them; the user decides whether they're acceptable, and can run a real check with the "Test Search" button.

### Agent

#### Added

- **Workers gain cross-work review capability**: added `search_all_worker_traces` (no need to know the intent_id in advance; globally searches steps across all work executions in the task by keyword) and `get_worker_trace` (after selecting a work, lists the step stream, searches in place by keyword, and fetches full content by step_id). These let workers reuse observations seen in other work but never written to a fact, avoiding duplicated effort.
- **`node_detail` made available to workers**: together with the review tools above, a worker can query a node's full details directly once it has the intent_id / node id.
- **Planning rounds triggered by `add_hint` are now explicitly announced**: previously a new hint was only folded into the overview for the planner to discover on its own. Now each `add_hint` records a "human added N strategic hints: …" trigger for the planner (a batch counts as one entry, no per-item spam), and the planner is explicitly told "this round was triggered by new hints" and sees the hint content directly.

#### Changed

- **Reworded the global-state hint in a "loosen up" tone**: encourages divergent exploration and timely reporting of cross-intent leads rather than premature convergence.
- **Streamlined worker boundary wording**: initial obstruction doesn't mean fully explored; focus on "exhaust the bypass techniques within this intent before concluding".
- `insert_assets` drops the `related` argument per asset: its only effect was deciding whether to add the asset to the task scope, and the value wasn't persisted (it was voided if the same asset was registered again, and the UI gave no indication of which assets were judged unrelated), so it merely forced the model to make an extra judgment that didn't stick.
- **Test scope (`task_scope`) is no longer affected by the asset-coverage toggle**: `insert_assets`' automatic scope additions (`source='auto'`) now run whether coverage is on or off, and `add_task_scope` remains available to the planner / in-task main Agent / goal-decomposition Agent. Scope is the task's authorization boundary and the filter baseline for asset queries; the coverage toggle should only decide whether to use it as the denominator for metrics, not whether to accumulate scope at all. Previously, turning coverage off disabled both the `auto`/`agent` write paths, leaving `task_scope` with only rows added manually in the UI. `list_untested_assets` is still hidden when coverage is off (it's a pure coverage-view tool).

#### Fixed

- **Fixed assets cross-contaminating between tasks** (#59): `list_assets` previously hardcoded the task id to 0 and queried the entire shared asset store, with no way for the model to express a scope filter, so it mistook other tasks' assets (especially IPs) for the current task's targets and skewed testing direction. It now returns only assets within the test scope (`task_scope`) of **this task and its directly related tasks**, matching by **ownership** rather than literal value: if the scope contains a root domain, all its subdomains/services/endpoints are reachable; if it contains a CIDR range, hosts and services in that range are reachable; fetching an out-of-scope asset by id also fails. Non-task contexts (Auto/pentest) have no scope to rely on and still fall back to the whole store. This also fixes the ownership blind spot where IP-direct hosts (e.g. `http://1.2.3.4/api`) couldn't be matched by a CIDR scope. The UI's "Test Assets" view (filtered by task producer) is unaffected.
- **Fixed the worker cross-work review tools being accidentally removed**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default tool set, an old migration that "narrowed the worker tool surface" unbound them again at startup, so workers actually couldn't get them. They've been removed from that unbind list, and databases that already ran the old migration get a one-time re-bind to workers.
- **Fixed occasional persistence failures of `/btw` side questions**: the side-question checkpoint now strips the NUL (`\u0000`) escape, which JSONB doesn't support, before writing to the database, avoiding write errors for content containing that character.

### Triggers

#### Fixed

- **Merged trigger sessions deduplicate task descriptions/goals by task**: when multiple tasks trigger together, the same task's description/goal is no longer concatenated repeatedly into the message, preventing long goals from piling up and bloating the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main Agent supports **multiple conversations**: create, switch, and reset context independently; every conversation is interactive.
- Supports persistent `/btw` **side questions**: follow up without interrupting the main thread; questions and answers are persisted and survive restarts.
- `spawn_task` adds `source_task_ids`, letting new tasks **read-only inherit** source tasks' assets and findings.

#### Changed

- Disabled workers' cross-engagement memory and narrowed their default tool surface: reading context and cross-work review belong to planning; workers only handle executing a single intent and writing back results.
- Streamlined the default worker prompt; `get_worker_output` drops the `terminated` and `worker_name` fields; trimmed the `insert_assets` / `list_assets` descriptions.

#### Fixed

- Fixed work terminating early due to **idle turns**: sometimes a model spends a whole turn emitting only thinking, with neither text nor a tool call. The harness then sees a natural end (`end_turn` with no `tool_use`) and finishes with `completed` plus an empty summary — an intent that wasn't finished breaks halfway, showing up as "task ended normally but with no text summary". None of the five LLM retry layers reach it: it's not an error, the same-provider safe-window retry triggers on "stream failure", the circuit breaker treats `err == nil` as success, and intent reruns only recognize `model_error`; the SDK's empty-response retry also misses it because that layer judges emptiness by "whether any event was yielded", and thinking deltas are events. The work Stop hook now recognizes such turns and injects a continuation instruction, letting the model proceed with the thinking it already produced — deliberately not "resend unchanged", since this kind of idle turn is usually determined by the prompt and context shape (a stable behavior, not a random glitch), and resending would only make the model think again.
- The count reuses the **empty-response retry count** from the LLM page's "Retries and Backoff" tab (both address "model finished but produced no substantive content", with different criteria and means), defaulting to 2; set `-1` to disable and revert to the old "idle turn ends it" behavior. It's a **total** cap for one intent, not a consecutive count: the harness already limits consecutive idle turns to a single nudge, and only a real tool-turn quota refreshes it, so this number guards against loops like "tool → idle → nudge → tool → idle" burning the intent's budget. When triggered, the log records `[work <worker> · #<intent>] idle turn…injecting continuation instruction (n/N)`. See `docs/LLM重试设计.md` §1.1.
- Fixed the context budget and input layout of `/btw` in long conversations; in non-secure contexts (non-HTTPS access), side-request ID generation falls back.

### Traffic

#### Added

- Findings can **link multiple traffic evidence items**, sortable, annotatable, and distinguishable by role (request / response / supporting).
- The report Agent **automatically links** relevant traffic evidence before writing the report.
- Added an Agent **traffic-binding toggle** and completed the handoff of evidence between Agents.

### Tasks

#### Added

- Supports **conversation-level finding retests**: retests run in a separate Agent conversation, with run status shown in the list and details.

### Interception

#### Added

- Approval records gain **details and execution audit**: you can view the tool request context, initial model/rule verdict, execution output, and argument fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the connection test not sending the custom session header, causing opencode zen to return 400.

### Networking

#### Added

- MCP's HTTP transport supports **skipping TLS certificate verification**, making it easier to connect to services with self-signed certificates.

### UI

#### Added

- Conversation list groups by Agent, with expand/collapse and independent pinning; conversations can be filtered by Agent.
- The attack-chain graph renders compacted (digest) nodes and collapses their members.

#### Fixed

- Fixed a blank screen caused by out-of-sync login credentials.
- Reworded intercept notices to clearly indicate "platform control", avoiding misinterpretation as a defense on the target side.

### Deployment and updates

#### Added

- Supports **one-click in-page updates**: the system configuration page gains a "Version and Update" card, and the top bar shows a notice when a new version is available. The flow is: download the release package → verify `SHA256SUMS` → smoke test → stage → exit so the guard script restarts the process to complete the swap, with the page refreshing automatically.
- Added guard startup scripts `start.sh` / `start.bat` as the official entry points (included in both release packages and Docker images; `install.sh` is unchanged). They decide whether to restart based on the exit code and forward SIGTERM to artex. Verification and swap logic all live in Go, keeping the scripts foolproof.
- Automatic fallback on failure: if verification or smoke testing fails, the package is discarded and the current version keeps running; if a new version fails to start three times in a row, roll back to the previous version. The settings page also offers manual rollback (note that the database schema is not reverted).
- Updates only accept GitHub domains and enforce HTTPS; the release source isn't configurable. One-click updates are disabled in development builds. GitHub query results are cached for 30 minutes so the top-bar notice doesn't exhaust the API quota.

#### Known limitations

- Under Docker, only the program is replaced, not the image: the toolchain isn't upgraded along with it, and recreating the container reverts to the image's bundled version; use `docker compose pull artex` when needed.
- The `skills/` directory from the release package isn't synced, so new built-in skills added in a version don't take effect automatically.
- Updating means restarting, which interrupts running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- Added a "Retries and Backoff" tab to the LLM page: both the **count** and **interval** of all five retry layers are configurable. A model-call failure passes through five layers from inner to outer: connection retries (SDK: connection reset / timeout / 429 / 5xx before the stream starts), empty-response retries (SDK: completed normally with no content, openai format only), same-provider safe-window retries (replay a broken stream before any output was delivered to the caller), rotation circuit breaking (cooldown and skip after consecutive failures reach a threshold), and intent reruns (the whole intent reruns after a worker ends with `model_error`); an inner layer is exhausted before the outer one is reached. Each layer has two knobs with unified semantics: leave blank = use the original default count and exponential backoff; fill in a count to use that count; fill in an interval to replace exponential backoff with a fixed interval; fill in -1 = disable that layer. The first three layers follow the endpoint and can override global defaults field by field in each model configuration (pinning only the interval still inherits the global count); circuit breaking and intent reruns are process-level semantics with a single global copy. Changes take effect hot after saving, no restart needed. Leaving everything blank reproduces current behavior, and old databases are byte-for-byte unchanged after upgrade (new columns default to 0 and absent settings keys mean all defaults). The connection test deliberately doesn't use these parameters — it has a hard 30s timeout, and stacking user-configured retries on it would only make a working endpoint test as a timeout failure. See `docs/LLM重试设计.md`.
- Each LLM configuration supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries this HTTP header with the current run's session id as its value (`conv-<id>` for chat conversations, `exp<x>-worker-i<intent>` for workers, etc.), for gateways that do prompt caching / sticky routing based on a session-id header. It's implemented by reading the session id from the request context and injecting it, with no changes to norma; the same shared provider can send different header values per session. Old databases get a migration, and the frontend LLM configuration dialog gains an input field.

#### Fixed

- Fixed saving an LLM configuration throwing `23502` when the session-header field was empty: the column is `NOT NULL DEFAULT ''`, but `NULLIF($n,'')` was mistakenly applied, writing an unset session header as NULL and triggering the not-null constraint. The parameter is now passed directly with empty-string semantics.

### Agent

#### Added

- Wall-clock timeouts now **wrap up in place**: with norma v0.3.6, when `MaxDuration` expires it interrupts the running tool and continues on the live ctx for the wrap-up turns (writing back recognized content + a summary, with a terminal state of `timeout`). It no longer relies on the worker/planner's external hard ctx of `maxDur+90s` to kill a stuck run outright as `aborted_tools`. Chat uses the same harness and gets the same behavior automatically; mainagent has no `MaxDuration` and is unaffected.
- Stall fallback: when the planner wakes on a heartbeat / no-change event and the graph has no open or running intents at all, its opening message becomes a stall alert, explicitly stating that no worker is running and no direction is queued, and that this round must produce one or more mutually distinct new intents (it may not produce zero intents).

#### Changed

- Refactored the worker prompt: the intent / launch instructions / raw anchored-asset JSON move into the system prompt, reassembled every round and never compressed away by compaction, with continuation not depending on the transcript's first message surviving. The launch user message is trimmed to just the global-state overview (degradable, tolerant of staleness). The trade-off is that per-intent data is mixed into the system message and cross-intent cache reuse is lost — a deliberate choice to "never lose the intent".
- Streamlined the default planner / worker text and fixed several real-world issues: the planner distinguishes `recent_done` by status (for blocked/exhausted it checks the trace before deciding — neither treating them as dead ends nor blindly rerunning), rewords negative conclusions as "observation / tentative, not conclusive" and checks evidence before relying on them, must produce intents when the goal isn't met and no open/running intents exist (a hard floor), and prioritizes depth over coverage. Workers write negative conclusions only as "observation + tentative reading", leaving the verdict to the planner, and put cross-intent leads into the fact summary for the planner rather than chasing them themselves. Reseeding appends the new defaults as a new version and switches to it; user-customized and old versions remain in history and can be rolled back.
- Narrowed the default tool set and asset write-back hints for work agents: workers only handle executing and writing back a single intent, while reading context / cross-work review belong to planning, so `list_facts` / `node_detail` / `list_companies` and cross-work search (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) were removed from their defaults, leaving only `list_findings` (dedup check before reporting a finding) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`. Stale field notes in the prompt that conflicted with the `insert_assets` schema (`type=tech`/`on_url`/`props`) were deleted. Old databases get a one-time migration stripping the corresponding bindings; same-named bindings on planner/main are untouched.

### Exploration graph

#### Added

- **Cold-node compaction in the exploration graph (cold-digest)**: folds old, long-inactive intents / facts into digest nodes shown in `graph_overview`. Original nodes are preserved permanently and can be fully restored by id (lossless storage, presentation-only compaction, reversible folding). Hot/cold classification uses reverse reachability — any live branch makes a node hot — with R=6 rounds of debouncing and connected-component grouping. Background compaction (minor folds uncovered cold blocks, major re-compresses from source to merge fragments) includes activity re-checks and cooldown mutual exclusion, never touching the hot path or overwriting revived nodes. The overview provides `cold_digests` and an asset-indexed `cold_index`, with `expand_digest` / `expand_index` handling restoration. Related / inherited tasks reuse their own folded views in the overview, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are given only to the planner and main agent, not workers. All include old-database migrations.
- `graph_overview` now emits the full `finding_list`: findings are a task's highest-value output and usually few per task, so they're all included in the overview (unlike facts, which are limited to a recent window). The planner/worker can see every confirmed finding at a glance each round without calling `list_findings`. Each is trimmed to `{id, summary, evidence?, from_intent?, assets?}`, with affected assets given as readable content (url / domain / ip:port) rather than raw ids.

#### Changed

- `graph_overview` no longer flattens `hosts`: the coverage block drops the host list (large-scope tasks carried up to 500 host strings per round with little value for planning decisions) and keeps only `host_count`; specific hosts can be queried with `list_assets` on demand. Added a top-level `done_intents_total` (total finished intents) alongside `recent_done_intents`, which is truncated to ≤15, so the planner knows whether the list was cut when deduplicating.

### Tools

#### Changed

- `add_company_scope` is now bound to the planner instead of the worker by default: defining a company's asset scope is the responsibility of planning / main control / Auto, while workers only execute exploration. New databases seed the default binding as mainagent/planner/auto, and old databases get a one-time migration.
- `list_assets` is now bound to the planner by default: it now has both `list_assets` (DSL search across the whole store) and `list_untested_assets` (untested within scope); old databases get a one-time backfill that doesn't override user unbindings.

### Tasks

#### Added

- Added a "Running Workers" column to the task list: counts intent nodes with `state='running'` for the task, using exactly the same definition as "Running Workers" in the task detail overview; shows 0 when none are running.

### Networking

#### Added

- Added a **global egress proxy** configuration: all target traffic can leave through one global proxy (http/https/socks5, with `user:pass` support). When traffic capture is on, it acts as the upstream of the MITM recording proxy (traffic is still recorded, then exits via the proxy; both intercept and pass-through paths use the upstream and don't leak the source IP). When capture is off, it's injected directly into the agent's bash environment and WebFetch (`proxyEnv` gains `ALL_PROXY` with socks5 support). The configuration lives in the settings KV table (no migration needed), and the frontend system configuration page gains a "Global Proxy" card, independent of the web-search proxy and LLM proxy.

### UI

#### Fixed

- Corrected the semantics of intent status labels: `exhausted` "Exhausted" → "Budget Exhausted" (it actually means stopped mid-way on reaching the step/time budget with only partial results written back, not that the direction was fully explored), `blocked` "Blocked" → "Execution Error" (it actually means model / API / network failures exhausted retries and the intent barely explored anything, not that the target / WAF blocked it), and added the previously missing `stopped` "Stopped" (work stopped manually by the user).

### Dependencies

#### Changed

- Upgraded norma to v0.3.4: MCP tool output gains truncation and disk spill (see `651b961`), and later v0.3.6 supports in-place wall-clock wrap-up.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Added an "Output Cap" to model configurations, with a choice of which request field name to use for it: the cap limits how many tokens a single reply may generate and is sent with every request; 0 (default) = don't send the field, letting the server default apply. It's distinct from the "Context Window" — the latter is the model's total capacity, used only locally to compute compaction thresholds and never appears in requests. The field-name switch only matters for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which most compatible gateways accept; OpenAI's official reasoning models (o-series / GPT-5) conversely only accept `max_completion_tokens` and reject `max_tokens` with `unsupported_parameter`, so such endpoints need to be switched manually. The other two formats have fixed field names (Anthropic uses `max_tokens`, Responses API uses `max_output_tokens`), so the switch is disabled for non-openai formats and cleared on save. Also wired up a previously disconnected path: the planner / worker / chat / main Agent / goal decomposition never set an output cap — openai-format calls didn't send it at all and Anthropic fell back to the SDK's 8192 — so merely adding the config without wiring it would have made the number ineffective. The value is now resolved from configuration each round just like "Streaming Output", and takes effect the round after a failover switch. Old databases behave exactly as before after upgrade (new columns default to 0 and an empty field name).
- Clicking "Test" in the backend now logs each HTTP attempt's status code and the gateway's raw response body (body truncated to 4K) to the server log, making it easier to diagnose 401s, quota messages, empty frames, HTML pages returned, etc. instead of only seeing the UI's collapsed ok / err. This logging doesn't depend on the "LLM Records" toggle.

#### Changed

- A task's LLM configuration chain can now be modified in any status, with no restriction to "running / paused / chain exhausted": after a task ends (done / failed / timeout) the main Agent conversation still uses this chain, and previously a broken model on the chain could neither be fixed nor worked around. Terminal-state blocks were removed from both the backend HTTP layer and the DB transaction, and the frontend dialog now allows editing and saving for terminal tasks as well. Saving a terminal task no longer reopens quota-blocked intents (those would be moved to `open` with no worker to execute them and no longer satisfy the "rerun intent" condition); to keep working, use "rerun intent" / "add goal" instead, which bring the task back to a running state.

### Agent

#### Changed

- "Send a message to a running Worker to steer it" now reuses the existing pause/resume + transcript-resume mechanism, matching main-Agent conversation behavior: previously it used a bespoke intervention-persistence protocol that touched scheduling barriers, resume flow, and a dozen-plus activity query filters. Messages are now injected through the next round's input, and the intent runs directly in a dedicated goroutine — not limited by the 3-slot worker pool and starting as soon as sent. The frontend keeps the message box and restores the "Continue Directly" button, with sending carried over SSE. No DB schema changes. Trade-offs: messages are memory-only with no crash recovery, and sending one may momentarily put the task one worker over its concurrency limit (acceptable for a low-frequency scenario).

### Tasks

#### Fixed

- Fixed out-of-memory (OOM) crashes when archiving large tasks: archiving now writes snapshots in a streaming fashion instead of reading the whole task into memory at once. Three recovery gaps on the cold-archive path were also closed — in modern installations traffic lives only in SQLite, and previously a crash between the PostgreSQL commit and the SQLite commit left an archived task's traffic stranded in hot storage with no way to recover it. A staging journal is now written whether or not a legacy directory exists, and a missing `journal.json` is treated as discardable.
- Fixed system sluggishness with many tasks: loading conversations and the UI showed noticeable delays. Queries for the task list, task context, and exploration records were optimized together, and the frontend dashboard, conversation page, and task detail page were updated to reduce redundant requests.

### Assets

#### Changed

- Removed the "256 rules max" limit on company asset scopes: companies that enter scopes IP by IP / domain by domain easily hit this cap and then had to split into multiple companies, while the scope itself isn't slower just for having more entries. The limit was removed in the frontend, backend, and demo mock alike; individual rules are still capped at 1024 characters, and the 2 MiB request-body cap remains as a backstop (roughly forty to fifty thousand rules).

### Skills

#### Fixed

- Fixed uploaded skill archives failing with "Upload failed: zip: unsupported compression": the Go standard library only includes Store / Deflate decompressors, so bzip2 and Zstandard archives written by compression tools at non-default settings couldn't be opened. Both decompressors have now been added (pure Go, no new external dependencies). For methods that genuinely can't be handled — Deflate64 / LZMA / XZ / PPMd, etc. — and for encrypted archives, a clear error is now raised before extraction, naming the file, the compression method used, and how to repack it, instead of dumping the underlying English error on the user.
- Fixed skill filename validation rejecting Chinese filenames as illegal: path validation previously used an ASCII allowlist `[A-Za-z0-9-_./]`, so a single Chinese-named file in the archive (e.g. `参考/说明.md`) made the whole upload fail with "archive contains illegal paths". It's now a Unicode denylist: filenames in any language and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments are still rejected, with directory-traversal protection unchanged. Skill names are relaxed the same way — ASCII is still limited to lowercase letters, digits, and hyphens (agentskills.io spec), while non-ASCII letters such as Chinese can be used directly as skill names, but spaces, dots, and path separators aren't accepted.
- Fixed garbled filenames or whole-archive rejection when extracting Chinese archives created by Windows compression tools: such zips don't set the UTF-8 flag and write filenames in GBK, so filenames are now decoded with a GBK fallback before path validation. In addition, quoted frontmatter such as `name: "中文技能"` is now parsed correctly to get the skill name.

### UI

#### Added

- Added a flat "All Findings" view on the findings page and made it the default, switchable with the original "Grouped by Task" view via header tabs. The grouped view requires expanding each task to see findings, which is a detour when you just want to scan a list across tasks. The flat view is one large cross-task table (10/20/50/100 rows per page) with behavior identical to the grouped view — select-and-export, inline expansion to view evidence and the detailed report, inline edits to name/category/severity, disposition changes, deep-dive, and deletion. Both views share the stats cards and filter bar; switching doesn't lose filters, and the current view and filter selections are stored locally. Polling hits only the current view, and inline edits are written to both caches so switching over never shows stale data.
- Added a "By Asset" view on the findings page: an asset tree on the left (company → root domain / IP → subdomain → service → endpoint; the company level appears only when an asset actually has an owner) and findings for the entire subtree of the selected node on the right, sharing the same table and filter set as the other two views. The tree only includes assets that have findings, with ancestor chains filled in as needed (when a finding is attached only to the deepest endpoint, the upper levels are still reconstructed). Counts on nodes are subtree aggregates deduplicated by finding; findings whose asset was deleted or that never had one go into an "Unassociated Assets" bucket. Unlike the other two views, the asset view doesn't poll — queries happen when entering the view, changing filters, adding/editing/deleting findings in the page, or clicking the tree's refresh button; the left tree is navigation and needn't be recomputed every 5 seconds. Each tree level shows only the increment relative to its parent (subdomains drop the root-domain suffix, services show `https :443`, endpoints show only the path), with full values in hover tooltips and breadcrumbs. When there are too many nodes, entire endpoint/service levels are dropped with a notice, while their counts still roll up into the parent.

#### Fixed

- Fixed conversation records being squeezed and unreadable in mobile conversation details: on narrow screens the conversation list collapses to give width to the records themselves.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: added `Config.MaxTokensField`, allowing the output cap for OpenAI Chat Completions to be sent as `max_completion_tokens` (reasoning models only accept this key). The two keys are mutually exclusive and only one is sent; the default remains `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security advisories.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "Non-streaming" toggle reverting to "Streaming" after being saved and reopened (#69): the configuration list endpoint's DTO omitted the `streaming` field, so responses never included it and the frontend fell back to default streaming on reading `undefined`. Writing and DB storage were fine; it just couldn't be read back. The DTO now includes the field (without `omitempty`, so `false` must appear in the response).
- Connection testing now verifies the model actually replies, using the configuration's real send/receive mode (#65): previously it only checked HTTP success, so a request that went through but got zero reply from the model (thinking burned the budget / the safety policy swallowed the text / a compatibility layer dropped `content`) still reported "Connection successful", at odds with the "no reply" behavior in conversations. Also, the test always used streaming, so a non-streaming configuration was actually tested on a different path. An empty reply is now a failure, a successful test shows the model's reply in the notice, and the current streaming toggle is carried into the test so "test passed" matches "conversation works".

### Agent

#### Changed

- `list_facts` now uses pagination + keyword filtering to avoid a single call bloating the context when there are many facts (#74): returns the latest 20 by default, supports `limit` (max 100), a `before` cursor, and a `q` summary keyword, returning `{facts, total, has_more, next_before}`; overly long individual summaries are truncated by character count (full text still via `node_detail`). Worker / planner prompts are updated to the pagination semantics. Old databases get a one-time migration refreshing the new parameter schema into the tool catalog table (`SeedTool` inserts only on first insert, otherwise the tool management page shows "No parameters").

### Tools

#### Added

- Added tool call-count statistics to the "Tool Execution" page (#72): the "Stats" button in the toolbar opens a dialog showing, per tool, call count, share, and failure count (descending by count). It reuses the list's task / keyword filters and aggregates the whole result set rather than the current page, fetching only when the dialog is opened.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixes silently dropped reasoning/refusal in OpenAI-family responses (Responses now adds `reasoning_text`), deduplication of three names for the reasoning field, bounded retries for empty responses (zero content blocks), and gateway 400s caused by compaction boundaries cutting tool pairs (also fixing existing orphans already baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM configuration supports a streaming/non-streaming toggle (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, one-shot full JSON), which can work around poor SSE implementations in some gateways (empty frames, dropped thinking fields), at the cost of losing live progress and real-time token counts during a run. Worker / planner / mainagent / chat / goals all resolve the value dynamically from the currently active configuration; the three provider wrappers — `llmpool` / `llmrec` / task runtime — are all non-streaming compatible; `llm_profiles` gains a `streaming` column with an `ALTER` backfill for old databases (default `true`, old configs upgrade seamlessly).
- Added support for the OpenAI Responses API format in LLM configurations: a third format `openai-responses` (calling `POST /v1/responses`) alongside Chat Completions / Anthropic; `BaseURL` normalization and a default model of `gpt-5`; the `format` constraint on `llm_profiles` includes `openai-responses` with an idempotent migration for old databases; the frontend format dropdown gains "OpenAI (Responses API)". Dependency upgraded to norma v0.3.1 (including the `reasoning_content` return fix).
- Records the raw HTTP bodies of LLM requests/responses: captures the real wire body at the HTTP transport layer, preserving tool schemas, `tool_use` blocks, and raw SSE frames that the normalized view hides. Each attempt of norma's internal retries is kept separately. `llm_records` gains `raw_request` / `raw_response` columns with an `ALTER` backfill for old databases; the recording page's detail panel gains a "Raw" view toggle and request/response copy buttons (with an `execCommand` fallback for non-secure contexts).

### Conversations

#### Changed

- Conversation-list multi-select becomes a "Multi-select" mode toggle: the list no longer shows a checkbox on every row by default (cleaner look); the header becomes "N total + Multi-select" button. Clicking "Multi-select" enters selection mode (checkboxes, select all, bulk delete); "Done" exits and clears the selection, and a fully successful bulk delete exits automatically (if any fail, it stays in selection mode for retry). Single-item rename / pin / delete still use each row's ⋯ menu.

### UI

#### Changed

- Enhanced task management, conversation actions, and traffic viewing (#57): task-list column sorting preferences can be persisted, inline renaming is triggered directly by an icon (no menu), and sheet interactions and traffic-viewing details are refined.

#### Fixed

- Worker asset tags show only domains and IPs, with empty-tag cases handled correctly.

### Agent

#### Added

- Added "quantitative acceptance verification" to the planner's goal checks: when a goal contains measurable acceptance criteria (asset-testing coverage reaches X%, N flags obtained, a certain privilege acquired), `prove_goal` must first verify the actual values in `graph_overview` (`coverage.pct`, findings count, etc.). If they fall short, `prove_goal` is forbidden and intents must be issued to close the gap; it may not mark the goal `met` early on the grounds that "the main part is done / mostly achieved". This fixes the case where a goal required 100% coverage but was marked complete at only 40%.

#### Changed

- Rewrote the planner prompt's criteria for "0 intents this round": the original described 0 intents as "the most common and most important principle", which made planners stop too early when the goal wasn't met and untested surface remained. It now says 0 intents are appropriate only in two concrete cases — (1) every direction you can think of is already covered by open/running/recent_done intents; (2) the next step depends on the output of work currently running and that output hasn't appeared yet (wait for it to finish and replan on the next wake after the graph updates). A converse constraint was added: when there genuinely is an uncovered direction that doesn't depend on running work, or the goal isn't met and untested surface remains, don't stop just because "0 intents is common".
- Strengthened the "evidence threshold for negative conclusions" in the worker prompt: for negative conclusions like "not injectable / port closed / no login entry" that could make the planner abandon an entire direction, workers must exhaust reasonable means within the intent before concluding (try different encodings / parameters / paths / methods). If means are incomplete or evidence is weak, always mark `confidence=inferred`, avoiding a rash `observed` negative that welds the whole route shut (early wrong negatives especially skew direction and are hard to self-correct).
- The above planner / worker default-prompt changes are appended as new versions and switched to current via one-time migrations (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag). Old versions remain in version history, and users who customized prompts can restore them from the version records.

### Operations

#### Added

- Added `reset-password.sh` to reset the administrator password (username fixed as `ARTEX`): supports both local and Docker deployments; connection details can be given explicitly or read automatically from `--dsn`/`$ARTEX_PG_DSN`/`config.json`. It uses `pgcrypto` in the database to generate a bcrypt hash compatible with backend login and writes it back to `settings.auth.password_hash`; no service restart is needed after a reset. The password is passed via an environment variable (never in process argv) and escaped to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting multiple tasks in the task list, their category can be changed in bulk; the target category supports "Uncategorized" to move tasks out of their current category. The whole batch is written in a single transaction, and the only possible failures are tasks that were deleted after being selected.

#### Changed

- The "Task Category" field on new tasks changes from a dropdown to a searchable input: typing filters existing categories, and pressing Enter (or clicking "Create" in the dropdown) on a name not in the database creates and selects it immediately; the selected item shows as a removable tag. Still limited to one category.

### Conversations

#### Fixed

- Fixed messages being blocked with "LLM not configured, cannot chat" when a conversation had an LLM configuration selected but no global active configuration. The pre-send check originally looked only at the global active configuration while actual execution prefers the conversation's selected one — inconsistent resolution logic. Both now use one set of rules (conversation/Agent binding first, global as fallback).
- The "cannot chat" notice is now state-specific: with no configurations at all it says "Add a configuration"; with configurations but none active it says "Activate one or assign a configuration to this conversation" — instead of always showing "Not configured", making problems easier to pinpoint.

### Agent

#### Added

- In tasks whose goals are all met, intents issued by the main Agent via `add_intent` can be claimed and executed directly by Workers, stopping when done. The Planner doesn't run at this point (no open goals means no planning), avoiding it re-judging the goals as met and cancelling the just-issued intent; once the frontier is drained the task returns to completed. Before issuing an intent, the main Agent asks the user whether to register it as a formal goal based on its content; registering resumes normal autonomous planning.

#### Changed

- When `step_ids` passed to `get_worker_trace` / `get_task_worker_trace` exceed the per-call limit (5), it no longer errors outright: it returns the full content of the first 5 steps and reports via `returned_step_ids` / `omitted_step_ids` / `notice` which were fetched and which weren't, with a hint that "no further fetch is needed if that's enough". Duplicate and invalid ids are deduplicated and dropped before counting.

#### Fixed

- The task list now sorts by task id descending (newest first) instead of by creation time. Tasks created at the same moment had identical timestamps and unstable ordering, which, combined with 10-second polling and random iteration of an in-memory map, made them frequently swap positions in the list.

### Assets

#### Fixed

- Fixed company-ownership recomputation being broken by a single dirty row: when `assets.ip` held a hostname (written by the Agent or the asset API), `a.ip::inet` threw `22P02`, making every create/update of a company asset scope and every company deletion fail and roll back. A safe conversion `try_inet()` is used instead, skipping invalid values rather than aborting the whole statement.
- Unparseable `ip` values are no longer silently skipped: after saving a company scope, an explicit notice reports how many assets have an `ip` that isn't a valid IP (with specific ids and values), warning that those assets won't be attributed by IP/CIDR rules. The same information is written to the server log, covering paths with no frontend response (company deletion, ScopeSentry sync, Agent writes).
- IP/CIDR matching for task test scopes also switches to `try_inet()`, replacing the old regex guard that only checked the character set (hostnames like `abc.def`, made entirely of hex letters, previously slipped through and triggered the same error).

#### Changed

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return per-item errors with `index` and give the fix directly (use `type=subdomain` with `domain` instead, or resolve A/AAAA records first), so the Agent can correct and reinsert on its own. Other assets in the same batch are unaffected and stored normally.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- Running Workers support pausing, resuming, and cancelling individual items; pausing preserves intents, facts, and findings, while cancelling transactionally cleans up the current intent and its direct outputs after execution exits.
- Added a `paused` intent state, execution barriers, and named termination reasons to prevent late blackboard writes after pausing, cancelling, or task deletion.
- Added an optional task concurrency limit; creation, resumption, finding deep-dives, and queue backfilling all go through a single persistent FIFO admission path.

#### Changed

- The default wall-clock duration of a single Worker run changed from 600 to 1200 seconds; a revived timed-out task restarts the clock on its next real execution.
- The real execution states of Workers, the Planner, and the main Agent uniformly drive the task status badge, fixing the "Idle" badge showing while a Worker was running.
- Worker sessions keep per-item controls and a current-session token summary; the model name moves to the right of the current session title.

#### Removed

- Removed the Worker multi-select, select-all, and bulk pause/resume UI, along with the Worker bulk-control API and mock contract.
- Removed the token badge and tooltip from Worker list rows; the underlying complete token ledger and task aggregation endpoint remain.

### LLM

#### Added

- Tasks support ordered LLM configuration chains; when a provider's insufficient quota is detected explicitly, the next configuration is switched to automatically, persisting the current configuration, exhaustion state, and error summary.
- Running and paused tasks support editing the full chain, reordering, and manually switching the current configuration, effective from the next LLM call.
- Automatic switches, manual switches, and full-chain exhaustion are written as structured system activities and surface deduplicable notices through the task activity feed.
- Added a task-role model resolution endpoint that uniformly resolves the configuration and model used by the Main Agent, Planner, and Worker on their next call.
- Added an optional global LLM Pool, supporting call order, per-model failure fallback, health state, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger aggregating input, output, and cache tokens by task, conversation, model, and configuration.

#### Changed

- Goal decomposition, the Planner, Workers, and the main Agent share a task-level LLM runtime (resolution priority below).
- Failover responds only to explicit quota, balance, or billing errors; ordinary rate limits, authentication, network, server, and context errors don't trigger a switch.
- The LLM configuration page becomes a card list with editing in a right-side drawer, providing Pool rotation order, priority, exclusions, health state, and recovery actions.
- Chain-exhaustion information wraps on narrow screens; the current model changes from a conversation-list icon to a text label next to the current session title with a full tooltip.
- LLM resolution order for roles within a task is now "Agent binding → task chain → global": a role with an explicitly bound model always runs on that model; unbound roles fall to the task chain, and to global when the chain is empty.
- An empty outbound proxy means direct connection; it no longer falls back to `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected). Proxy input supports `socks5://user:pass@host:port` with credentials.

#### Fixed

- Transient stream failures before submission (before any output is produced) are safely retried with backoff on the same provider, significantly reducing cases of "stopped after one or two tools, no summary, terminal state `model_error`". Quota exhaustion, overlong context, and deterministic 4xx errors aren't retried and are still handled by failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon from the conversation list to avoid confusion with Worker status icons.

### UI

#### Added

- Tasks support creating, renaming, deleting, and filtering global categories; new tasks can select a category directly.
- Added global task-template CRUD with a right-side management drawer; new tasks can load preset descriptions and goals, or save current content as a template.
- New tasks can link multiple source tasks and multiple company asset scopes; company scopes are recognized automatically from a single multiline text box for domains, URLs, IPs, CIDRs, ICP numbers, and company keywords.
- Task test assets support live addition and deletion, recording sources such as manual, company, inherited, or Agent-discovered; Worker session titles show the current test assets and a source summary.
- The findings page groups by task with independent pagination within each group, letting you write a description for a finding and create a high-priority deep-dive intent.
- Conversations support rename, pin, unpin, and delete; the task list supports bulk pause and resume on the current page.
- Traffic details and tool-execution details move to right-side drawers; HTTP messages now include `Host` and highlight the request line, status code, headers, JSON, and markup-language bodies.
- The task list's action column gains "Details" and "Pause/Resume" buttons; the conversation send key can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and the web-search area gains a proxy input.
- The badge to the right of conversation titles now shows the LLM configuration name, with the model ID moved to a hover tooltip; tasks can have an optional name, and the task list gains a name column and search (falling back to the description when empty).
- The skills page adds usage counts, last-used times, and missing-dependency statistics, making it easier to locate skills that aren't taking effect or aren't installed.

#### Changed

- Task titles become focusable detail links; task assets, company assets, finding task groups, and findings within groups use real server-side pagination, stable sorting, and exact totals.
- Adding a company now uses a right-side drawer; scope entry is unified as multiline text with live recognition, validation, and type preview.
- The main Agent input supports auto-growth, `Enter` to send, `Shift+Enter` for a newline, and avoids accidental sends while an IME composition is in progress.
- Agent previews, task reports, and related details uniformly use the shared Markdown renderer; delete confirmations, long errors, and mobile drawer widths are fixed consistently.
- The task-template selector displays and searches by template name but submits the template ID; the original system font size is restored and the application version is shown uniformly as `0.3.3`.
- Task totals and current-session tokens highlight only input, cache-read, and output figures; the send button uses a simpler up-arrow icon.
- The task overview's test scope shows company names instead of IDs.

#### Fixed

- Ordinary text containing dots is no longer misidentified as an ICP filing number.
- Fixed name collisions in the variable catalog with global runtime variables (e.g. `{{.Now}}`) that caused duplicate keys in the Agent editor's variable list.

#### Removed

- Removed the separate "Enter" button on task cards; clicking the task title now consistently opens the details.
- Removed the dashboard "New Task" button, the Logo URL field when adding a company, and the finding-count badges at the top of the findings page and in task groups.
- Reverted the global 10% font-size increase and removed the Worker selector, model icons, and row-level token stats from the conversation list.

### Agent

#### Added

- New tasks can directly link multiple existing tasks, read-only inheriting the goals, facts, findings, finished intents, asset scopes, and blackboard context of the direct source tasks in real time.
- Blackboard read tools support querying source-task nodes, facts, findings, and execution traces on demand; inherited nodes carry source markers and all write tools refuse to modify them.
- Company-scope tools support domains, URLs, IPs, CIDRs, ICP numbers, and company keywords; keywords are only hints to the Agent about scope and don't participate in automatic asset attribution.
- A finding deep-dive creates a high-priority manual Worker intent with asset anchors and a `derived_from` edge in the original task.
- Added a "Goal Management" card to the overview for manually viewing, adding, editing, and deleting goals. Adding or editing notifies the Planner and revives the task; deleting hard-deletes the goal node (cascading to edges and anchors) without reviving.
- The main Agent gets the `steer_work` tool by default, able to inject real-time corrective instructions into running Workers without interrupting them or losing progress (still verifies the intent belongs to the task).

#### Changed

- Source-task intents don't enter the new task's frontier; the new task keeps its own exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: it now stops at `stopped`, requires a deletion reason, attaches the reason as a fact to the intent and writes it into the payload, and the planner perceives it as a `cancelled` trigger (preserving the intent content and reason).
- Main-Agent conversations are now driven by a server-side activity feed; a failed send restores the input. Opening a conversation auto-scrolls to the bottom and keeps the last reply visible after details load lazily.
- Pausing a task terminates current Main Agent, Planner, and Worker calls, but doesn't prevent the user from starting a new main-Agent orchestration session while paused.
- Cancellation, closing, and stream interruption uniformly preserve the real termination reason, content already generated, run turns, duration, tokens, and tool calls not returned.

#### Removed

- Removed client-side optimistic message echo for the main Agent to avoid duplicate messages and cross-conversation streaming under pauses, failures, or concurrent activity.

### Constraints

#### Added

- The goal-decomposition phase extracts operation constraints (allow/deny) before decomposing goals; the main Agent can add more at runtime. Constraints are injected at the highest priority into the Planner and Worker system prompts, and after debiasing, diversity and breadth-first exploration explicitly obey them.
- Added a "Constraint Management" card to the overview with create/update/delete endpoints; constraint injection can be toggled separately for Planner / Worker (both on by default, read each round, immediately effective). The new `task_constraints` table uses `CREATE TABLE IF NOT EXISTS`, so old databases get it automatically.

### Interception

#### Added

- Command interception adds model-fallback approval alongside regex/string rules: when no rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, with configurable fallback-on-failure and approval-timeout actions. The interception page splits into "Interception Rules / Model Configuration" tabs, and model verdicts are labeled with the `[模型]` prefix and a reason.

#### Fixed

- Hardened judge output parsing so a correct `DENY` verdict isn't mistaken for an allow.

### Traffic

#### Changed

- Traffic recording now stores each entire exchange in SQLite (large bodies spill into a hash-deduplicated blob bucket) with a trigram full-text index on text bodies, supporting arbitrary substrings and Chinese-language search. Deletion degenerates to a single SQL transaction, dropping from hours to milliseconds without pausing recording in between. Added streaming downloads for large bodies and full-text parameters for `traffic_search`; old file-tree data needs no migration and remains readable, searchable, and deletable.

### Tasks and assets

#### Added

- Added an "Asset Coverage" toggle at task creation (on by default): when off, coverage isn't computed or displayed, the overview shows assets only, `task_scope` isn't auto-accumulated, and related tools are removed from the Planner / main Agent. Company associations are unaffected by this toggle.
- `insert_assets` gains a `related` flag per asset (default `true`), effective only when coverage is on. `false` puts the asset only into the shared asset store without counting toward this task's coverage (e.g. incidentally discovered side sites or unrelated assets).
- Company scopes and task test assets uniformly support text recognition of domains, URLs, IPs, CIDRs, ICP numbers, and keywords; domains/IPs can create or reuse global assets, while CIDRs/ICP numbers/keywords remain as task-scope context.

### Build

- `build.sh --release` builds Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one run, generating zip release packages with `skills/`, configuration examples, and the README.
- Use Go linker flags to strip debug info and generate zip release packages; UPX is now explicitly optional, avoiding segfaults at startup on some Linux environments caused by its self-extracting ELF.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and ships `SHA256SUMS` with the release packages.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks can link multiple existing tasks, read-only inheriting the facts, findings, finished intents, asset scopes, and blackboard context of direct source tasks in real time; the new task still uses its own execution queue, working directory, and conversation history.
- Tasks support ordered LLM configuration chains, automatically switching to the next configuration when insufficient provider quota is explicitly detected, persisting the current configuration, exhaustion state, and structured audit activities.
- Running and paused tasks support editing the LLM chain, reordering, and manually switching the current configuration; automatic switches, manual switches, and full-chain exhaustion are surfaced on the task detail page.
- Running Workers support pause, resume, and cancel. Pausing preserves blackboard data; cancelling transactionally cleans up the intent and its directly produced facts, findings, and execution records after the Worker stops writing.
- Deleting a task can optionally clean up associated assets, traffic, findings, and task files on disk, with added delete barriers, concurrency protection, and auditable deletion statistics.
- Added an optional task concurrency limit; new tasks beyond the limit are queued FIFO and start automatically when a run slot frees up.
- Added task-asset pagination, company-asset pagination, task-category statistics, and a paginated-intent mock contract.
- Added the `build.sh` single-binary build script with static frontend export, asset embedding, cross-platform targets, and build-version injection.

### Changed

- Explicit task-level LLM chains coexist with the existing global LLM Pool; explicit chains keep strict quota-failover semantics, while tasks without one follow Agent binding and global configuration rules.
- The main Agent input supports an auto-growing multiline field, `Enter` to send, `Shift+Enter` for a newline, and avoids accidental sends during IME composition.
- The Planner, Workers, and the main Agent share a task-level LLM runtime; the task chain uses the smallest context window among candidate models as the safe compaction threshold.
- The task detail page shows running / idle / paused based on real LLM call state; facts, findings, intents, asset references, and graph nodes from source tasks are uniformly source-marked and kept read-only.
- Agent previews, task reports, and related detail views uniformly use the shared Markdown rendering component.
- LLM model configuration uses cards and a drawer, with the model list scrolling independently inside the drawer.
- Task pausing no longer blocks main-Agent conversation: the orchestration session is independent of task pause, so new messages can still be sent while paused (pausing only terminates the currently in-progress round).
- The default wall-clock duration of a single Worker run changed from 600 to 1200 seconds.

### Fixed

- Removed optimistic echo from the main Agent console in favor of pure server-side data rendering, fixing mixed-up messages and content bleeding from other conversations on pause or failed sends.
- Opening a main-Agent conversation scrolls to the bottom and shows the last reply in full: once the last reply's full content loads lazily, it auto-sticks to the bottom instead of being pushed off-screen.
- Fixed the main-Agent conversation continuing to run after a task was paused, and the orchestration Agent not stopping the main Agent when pausing a task.
- Fixed task status badges and action buttons being out of sync when the task completed or the Planner, Worker, or main Agent was actually running.
- Fixed possible late blackboard writes or leftover files between Worker cancellation, task deletion, and concurrent writes.
- Fixed broken Markdown previews, long-text overflow in delete confirmations, mobile widths, and misaligned task-detail buttons.
- Added named termination reasons to all Agent cancellation paths; activity details show who cancelled, the terminal state, run turns, duration, token usage, and tool calls not returned.
- Fixed a race where the parent context could overwrite the named `shutdown` reason on backend shutdown, and garbled text from byte-based truncation of Chinese activity summaries.
- Fixed cancellation events with partial streaming output losing the real termination reason, while preserving content generated before cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
