# Experimental native Codex command Stop

This integration branch needs a native command-start barrier for GC's
`stop-commands` operation. Stock Codex turn interruption leaves Unified Exec
commands running and cannot satisfy this contract. The patch in this directory
extends the native owner; the BB provider continues to use generated supervisor
endpoints only.

Base: Codex 0.156.1, commit
`b412ff32c417f855c2b2d1581b77058eed87c84b`.

The experimental `thread/backgroundTerminals/stopTurn` request takes a native
thread, exact turn and explicit preserved process handles. It fences command
admission before interrupting, retains native handles before notifications and
waits for confirmed exit. An interrupted spawn without a retained handle is
reported as uncertain, never successful. Returning a tool handle does not
implicitly detach a service.

The GC runtime runs the native app-server and its TUI as one managed incarnation,
retains native ownership events, and exposes a private owner-only control socket.
The public API remains GC's supervisor. Main conversation identity comes only
from a correlated non-ephemeral native start/resume response; a temporary title
thread cannot replace it.

## Build and verification

Apply `codex.patch` to a clean checkout of the exact base commit. The patch
includes the experimental protocol export and reconciles only local workspace
versions in the release lockfile; external dependency pins are unchanged.
Use the pinned Rust toolchain and upstream build prerequisites. In `codex-rs/`:

```sh
just fix --locked -p codex-core -p codex-app-server-protocol -p codex-app-server
just fmt
just test --locked -p codex-core -p codex-app-server-protocol -p codex-app-server --lib --test-threads 2
cargo build --locked -p codex-cli --bin codex
```

Include the matching official 0.156.1 code-mode helper beside the CLI. The
Linux x86-64 helper's SHA-256 is
`45ba654b0e145406f3316be4729b886980ce811aa2e3b6ed4e033dd41ab524f1`.
The upstream source build of that helper currently requests a V8 archive that
returns HTTP 404; the candidate uses the unchanged, checksum-verified official
helper. This does not make other native versions compatible.

The live acceptance must also prove command-start races, no delayed write after
Stop, same-conversation resume, explicit service preservation, isolation from
other conversations, and a repeated old Stop during a newer turn. Unit tests or
an idle transcript do not establish these runtime guarantees.

Qualification is in progress. No stock installation is replaced by this recipe,
and this experimental source is not an approved stable release. Use a disposable
VM and record the exact CLI/helper/supervisor hashes with the live evidence.
