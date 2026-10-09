# Session Module

Read `REQUIREMENTS.md` before changing session behavior, tests, or callers that
depend on session lifecycle semantics. Read `PLAN.md` before changing session
ownership boundaries, deciders, command APIs, events, or extraction sequencing.
(`DESIGN.md` is the long-form architecture document; it has not landed yet and
is iterating under design-review bead `ga-unpr2y`.)

## Purpose

`internal/session` owns the session primitive: start, stop, prompt, observe,
identify, and project provider-backed sessions. Session state is bead-backed;
runtime state is observed through providers; work must survive session churn.

## Reconcile Rule

When code, tests, or docs disagree with `REQUIREMENTS.md`, do not silently pick
one. Reconcile them in the same change:

- If the requirement is right, update code and add or adjust tests.
- If current behavior is right, update `REQUIREMENTS.md` and cite proof.
- If the mismatch is ambiguous, stop and ask for the product rule.

New session behavior needs at least one scenario row in `REQUIREMENTS.md` with
evidence from tests, source, an issue, or a commit.

## Boundaries

- Use `ProjectLifecycle` and lifecycle helper APIs for read-side decisions.
- Inside `internal/session`, lifecycle transition/patch helpers may build
  mutations. Outside `internal/session`, call session-owned command APIs instead
  of applying patch maps or writing lifecycle metadata directly.
- Treat duplicated session decisions in API, CLI, worker, or runtime adapters as
  extraction candidates. Prefer moving the rule into `internal/session` and
  leaving the adapter to gather facts, execute actions, or render responses.
- Extract decisions one cluster at a time. Do not introduce a broad session
  facade before a narrow contract and tests prove it.
- Do not add production session lifecycle or identity metadata writes outside
  `internal/session`. Existing external `SetMetadata*`/bead mutation sites are
  extraction candidates unless they are tests, fixtures, or migration/doctor
  repair code.
- Keep provider-specific runtime behavior behind `internal/runtime`.
- Keep production CLI session creation and lifecycle operations on the
  `internal/worker/handle.go` boundary unless root `AGENTS.md` names an active
  exception.
- Do not make ordinary config names or `template:<name>` values act like live
  session targets.

## Startup Fencing Pitfalls

- `Store.Tx` groups rollback writes but cannot read, so it does not fence a
  stale creating snapshot against completion. `WithPendingCreateRollback` and
  `CommitStartedIfCurrent` share `WithSessionMutationLock` with incarnation
  allocation. Keep the fresh read and write inside that lock; provider calls
  and retired-session cleanup stay outside. This is an in-process fence, not
  protection against arbitrary external bead writers.
- `testing/synctest.Wait` does not consider `sync.Mutex.Lock` durably blocked.
  A test that pauses a lock holder on a channel and waits for a competing
  mutex waiter will hang. `store_start_fence_test.go` checks the shared lock
  directly at both read and write boundaries instead of relying on timing.
- `awake` is not proof that creation completed: the reconciler can heal a
  running, still-claimed creation to awake before provider Start returns
  (issue #6104 / PR #6105). Rollback must allow that state when its claim and
  identity remain current; active or a cleared claim still blocks rollback.
- A final atomic start commit may reject after the async refresh succeeded.
  Propagate that stale verdict to the provider-aware caller and clean up using
  the attempted identity outside the mutation lock. Persistence failure alone
  must not trigger runtime cleanup.

## Message delivery

Interpret runtime delivery outcomes in the shared session layer. Proven delivery
without an observed busy transition is successful delivery with a diagnostic, not
permission to resend or evidence of turn completion. Keep uncertain delivery as
an error so API and CLI callers cannot disagree or duplicate input.

## Transcript attribution

Preserve the provider session key through fallback discovery. For providers with
strict keyed lookup, an absent new transcript is pending history, not permission
to read an older conversation in the same directory.

## Runtime loss

A lost process is not an explicit conversation reset. Preserve committed manual
conversation identity during advisory healing; use the session-owned policy so
pool recycling and failed-create recovery remain distinct. Apply the same manual
conversation policy during wake-failure and churn accounting; retaining context
must not disable retry counters or quarantine.

The API marks manual conversations with `session_origin=manual`; do not assume
all retained conversations carry the legacy `manual_session=true` flag. Reapers
must honor the started-manual conversation policy as well as legacy exemptions.

Codex interrupt confirmation passes the stored native session key across the runtime boundary. A shared native home does not identify a conversation; never select another session's newest transcript to acknowledge a stop.

Owned-command Stop is an optional native runtime capability. Carry the persisted
native session key and the caller's explicit turn ID to that capability. Do not
substitute terminal interruption, session destruction, or OS process traversal
when command ownership or termination confirmation is unavailable.
An exact runtime incarnation can publish its native conversation key through a
correlated native start/resume reply. The session owner persists that proof under
its mutation lock and rejects replacement of an existing key. Subagent events,
filesystem recency and other runtimes cannot supply this identity. Before a
startup reply, a first-turn baseline may omit the key only when no turn started;
Stop/detach still require a bound conversation.

A failed resume, including immediate post-start death, must use the same
conversation-preservation policy as advisory runtime loss. A stale-key retry
is not authorization to reset a committed manual or always-named conversation.
