# The enboot protocol — version 1

**enboot** is a small resident process that keeps open file descriptors
alive on behalf of a **tenant** process that wants to replace its own program
image without closing them. enboot knows nothing about what the
descriptors are for.

This page is the whole protocol. Its vocabulary is: *enboot, tenant, name,
descriptor, path, pid, status*. Any other noun appearing here is a defect.

## Transport

enboot starts the tenant. Before it does, it creates one `AF_UNIX`
`SOCK_STREAM` socket pair; the tenant inherits one end as descriptor **3** and
finds `ENBOOT_FD=3` in its environment. There is no listening socket, no path on
disk, and no second tenant.

The conversation is **strict lockstep**: the tenant writes one request line,
then reads one reply line, and never has two requests outstanding. A message
carries **at most one descriptor**, as `SCM_RIGHTS` ancillary data on the
message's bytes. Lockstep is what makes stream sockets safe for this — two
messages are never in flight to be coalesced — and it is why there is no
framing beyond the newline.

```
request  = verb [ SP argument ] LF
reply    = "ok" [ SP argument ] LF  |  "no" SP reason LF
name     = 1..64 bytes of [A-Za-z0-9._-]
path     = any bytes except LF and NUL; absolute
```

A line is at most 65,536 bytes. An unknown verb is answered `no verb` and the
conversation continues; that is the forward-compatibility rule.

A tenant runs other programs, and none of them is a tenant. So the first thing
an image does with the descriptor is mark it close-on-exec, and the first thing
it does with the two variables is remove them from its environment; it puts
both back for one `execve` only — its own, into its next image.

## Verbs

| verb | carries | reply | meaning |
| --- | --- | --- | --- |
| `hello <n>` | — | `ok <m>` | The tenant speaks versions up to *n*. enboot answers *m* = min(*n*, its own highest); both use *m* from here. **Must be the first request of every program image** — it also tells enboot "a new image is now running and has not yet said `ready`". |
| `put <name>` | one descriptor | `ok` | Hold a duplicate of this descriptor under *name*. Replaces (and closes) any previous holder of the name. |
| `get <name>` | — | `ok` + one descriptor, or `no absent` | Send a duplicate of the held descriptor. **enboot keeps its own.** A tenant that dies one instruction after `get` has lost nothing. |
| `list` | — | `ok <name> <name> …` | Every held name, space-separated, unordered. |
| `drop <name>` | — | `ok` or `no absent` | Close enboot's copy. |
| `ready <path>` | — | `ok` | "The image at *path* is up and has everything it needs." *path* becomes the **rollback target**. |
| `next <path>` | — | `ok` | "When I exit, do not exit with me: run *path* in my place." One-shot. |
| `reaped` | — | `ok <pid>:<status> …` | Exit statuses of processes enboot inherited and reaped since the last call (see *Orphans*). `<status>` is the raw `wait(2)` status word in decimal. Platforms without re-parenting answer `ok` with nothing, always. |

## What enboot does when the tenant exits

enboot is the tenant's parent and learns of its exit from `wait(2)` — never
from the socket. Then, in order:

1. If `next <path>` was set: start *path* as the new tenant. Clear `next`.
2. Else, if the image that exited **never said `ready`** and a rollback target
   exists and enboot has not just rolled back: start the rollback target,
   with `ENBOOT_ROLLBACK=1` added to its environment.
3. Else: close everything and exit with the tenant's status.

Every tenant is started with the argument list and environment enboot itself
was started with, a **fresh** socket pair on descriptor 3, and enboot's
standard streams. Held descriptors are **not** inherited; the tenant asks for
them.

A tenant may also replace its image with `execve(2)` directly, keeping its pid.
enboot cannot see that and does not need to: the new image's `hello` clears
"ready", and rule 2 covers it if it dies.

## Orphans

Where the platform offers it (Linux: `PR_SET_CHILD_SUBREAPER`), enboot
declares itself a sub-reaper. Processes orphaned by a tenant's exit become the
enboot's children; it reaps them and reports each through `reaped`. Where the
platform does not, orphans go to the system's init and their statuses are gone;
`reaped` is always empty there.

## What is deliberately absent

- **No data path.** enboot never reads from or writes to a held descriptor.
- **No spawning.** enboot starts exactly one kind of process: the tenant.
- **No state on disk.** A checkpoint is the tenant's business; if it wants the
  enboot to carry one, it `put`s a descriptor of an unlinked file.
- **No download, no verification, no version policy.** *path* is given, not
  chosen.
- **No push.** enboot never speaks first.

## The complete list of reasons this binary could ever need to change

1. A defect in enboot itself.
2. A platform changes or removes `SCM_RIGHTS`, `socketpair`, `execve`, or
   `wait` semantics. (These are 4.2BSD, 1983.)
3. A new platform needs a different orphan mechanism.
4. The rollback *policy* (rule 2) turns out to be wrong — this is the only
   judgement enboot makes, and therefore the most likely entry on this list.
5. A tenant needs a ninth verb. `hello` negotiates it; an old enboot answers
   `no verb` and the tenant must cope. **This is the "compatibility gap".**
