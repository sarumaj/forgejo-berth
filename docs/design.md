# forgejo-berth design

forgejo-berth gives every waiting Forgejo Actions job a runner of its own, on a fixed host
where the job's memory fits. It registers an ephemeral runner (one job, then Forgejo removes it
and its token) and hands it to a container slot that was reset for that job alone. It learns
each job's memory peak from the slot after the job, and uses the history to pack later jobs onto
the hosts.

It fills a gap. Other autoscalers create machines on demand: cloud VMs per job, VM pools, or
Kubernetes pods scaled by the number of pending jobs. forgejo-berth is for hosts you already
have and pay for, whatever load they carry: it decides which job runs where, and when a job
waits because it would not fit.

## Goals and non-goals

Goals:

- One runner per job, registered just in time, deleted when unused. No long-lived runner token
  on a CI host.
- Admission by learned memory: a job starts on a host only when its predicted peak fits beside
  the reservations of the jobs already running there.
- Trusted owners may get runners with more powerful labels (for example host-mode execution);
  everyone else gets the shared labels only.
- One static binary (`CGO_ENABLED=0`): the broker, the slot's forced command and the reset
  cycle.
- Usable without a fleet of one's own design: one machine with Docker is enough to start.
- A shadow mode that computes and logs decisions without acting, next to a live instance.

Non-goals:

- Creating or destroying machines. Use a cloud autoscaler for that (see the README).
- CPU scheduling as the first rule. Memory is what kills jobs and stalls hosts, so memory is
  what berth admits by. CPU use is recorded for every job, and an optional rule admits by it
  too ([CPU](#cpu)); it is off by default.
- Replacing Forgejo's own job order. Forgejo decides which job a runner gets; berth predicts
  that choice (see [Claims](#claims-and-reservations)).

## Architecture

```mermaid
flowchart LR
  F[Forgejo API]
  subgraph broker[forgejo-berth run]
    L[look every interval]
    H[(history SQLite)]
    S[scheduler]
  end
  subgraph host[CI host]
    C[forgejo-berth slot<br/>forced SSH command]
    Y[forgejo-berth cycle<br/>one per container]
    K[Incus container<br/>runner, one job]
  end
  L -- waiting jobs --> F
  L -- status --> C
  L --> S
  S -- predicted peak --> H
  S -- register ephemeral runner --> F
  S -- assign uuid, token, labels --> C
  C -- assignment file --> Y
  Y -- reset, configure, start --> K
  K -- takes one job --> F
  Y -- task and peak in its state --> C
```

That is the `ssh` provider. With the `docker` provider, the slot side runs in the broker itself
and reaches the containers through the engine's API (the `incus` provider alike, through the
Incus API, its slots Incus containers restored from a snapshot):

```mermaid
flowchart LR
  F[Forgejo API]
  subgraph broker[forgejo-berth run]
    L[look every interval]
    S[scheduler]
    Y[a reset cycle per slot]
    D[(state_dir)]
  end
  subgraph engine[Docker Engine]
    R[runner container<br/>one job]
    E[the slot's own Docker]
  end
  L -- waiting jobs --> F
  L -- status --> D
  S -- assign --> D
  D -- assignment --> Y
  Y -- create, start, remove --> R
  Y -- create, start, remove --> E
  R -- job containers --> E
  R -- takes one job --> F
  Y -- task and peak --> D
```

The broker is one loop. Every `interval_seconds` it:

1. asks every host for its slots (`Provider.Status`). A host that does not answer is marked
   unreachable; its slots get nothing, its assignments are kept;
2. settles each slot that finished an assignment: an unused runner is deleted; a runner that ran
   a job is looked up (which of its candidate jobs had that task id, and how it ended), and a job
   that ended `success` or `failure` is recorded with its peak;
3. forgets assignments no slot reports any more (after 90 seconds), and at once those on hosts
   removed from the configuration; their runners are deleted;
4. reads the waiting jobs (`GET /api/v1/admin/runners/jobs`), prunes the history once a day,
   counts the runners settled unused against the jobs that still wait and backs those off that
   reached the limit ([Back-off](#back-off)), releases idle runners none of whose jobs wait any
   more;
5. schedules: for each waiting job not backed off, in arrival order, picks a slot and starts a
   runner there (register, keep the assignment, `Provider.Assign`);
6. publishes its state and metrics.

At start it sweeps the runners whose name has the configured prefix and that no kept assignment
holds (left by a crash between registering and keeping).

### Packages

| Package                    | Role                                                                                                      |
| -------------------------- | --------------------------------------------------------------------------------------------------------- |
| `cmd/forgejo-berth`        | subcommands `run`, `slot`, `cycle`, `check`, `pause`, `resume`, `version`                                 |
| `internal/config`          | the broker's YAML configuration, defaults and validation                                                  |
| `internal/broker`          | jobs, slots, assignments; offer, predict, schedule; the loop                                              |
| `internal/history`         | SQLite: finished runs and the assignments held across restarts                                            |
| `internal/forgejo`         | the Forgejo API client (admin and owner tokens)                                                           |
| `internal/provider`        | the slot provider interface and the SSH provider                                                          |
| `internal/provider/docker` | the docker provider: the Engine API client and a slot's system                                            |
| `internal/provider/incus`  | the incus provider: the Incus API client and an API `Instance`                                            |
| `internal/slot`            | the slot side: the forced command, the reset cycle (Incus), and the slot side run in the broker (`Local`) |
| `internal/web`             | `/metrics`, `/state`, `/healthz`                                                                          |

## Slot provider interface

```go
type Provider interface {
    // Status lists the host's slots: state, current assignment, and the last finished
    // assignment with its task id, peak memory and duration.
    Status(ctx context.Context, host config.Host) ([]Slot, error)
    // Assign hands a ready slot a runner. An error means the slot refused; the broker then
    // deletes the runner.
    Assign(ctx context.Context, host config.Host, container string, h Handoff) error
    // Release asks the slot to stop the assignment's runner if it has no job yet.
    Release(ctx context.Context, host config.Host, container, assignment string) error
}
```

A finished slot's peak and task id are part of `Status` (the `last` object), so a slot needs no
separate query after its job: the broker reads them at the next look, before the slot takes a
new assignment.

Three providers implement it:

- `ssh`: every host is reached as one user whose authorized key forces `forgejo-berth slot`,
  and a `forgejo-berth cycle` per container runs on the host. The protocol is below.
- `docker`: every host is a Docker Engine; see [Docker provider](#docker-provider).
- `incus`: every host is an Incus server reached through its API; see
  [Incus provider](#incus-provider).

The `docker` and `incus` providers need nothing on the host. Their slot side runs in the
broker's process (`slot.Local`): the same reset cycle per slot, and the same `status`, `assign`
and `release` as the SSH slot command, on the same state files, kept in the broker's
`state_dir`, one directory per host. Only the cycle's `System`, the container as the cycle
reads and changes it, differs.

### Where the slot state lives

The SSH protocol keeps a slot's state on its host: the assignment waiting (`<slot>.assignment`),
the release (`<slot>.release`), and the cycle's state (`<slot>.json`: its state, the
assignment it serves, the last finished assignment with its task, peak, duration and CPU). A
provider without a host-side agent keeps the same files in the broker's `state_dir`
(default: `slots/` beside the history), and not on the containers:

- the cycle needs to write its state often (every state change, every job), and the broker is
  the only reader. Docker's labels cannot change after a container is made, and an Incus
  config key would cost an API request per change;
- the last finished assignment must outlive the container: a Docker slot's container is
  removed at the reset, and an Incus snapshot restore would roll a key back;
- one writer and one format for all providers keep the broker's view the same.

The containers carry what identifies them: a Docker slot's containers and volume have the
labels `org.forgejo-berth.host`, `org.forgejo-berth.slot` and, for the runner,
`org.forgejo-berth.assignment`.

A broker that stops leaves the slots as they are: the cycles end after their step, and no
container is reset. At the next start each cycle **resumes** from its state file: it keeps the
last finished assignment (which the broker may not have read yet), and takes up the assignment
it served. A runner still active is watched to its job's end; one that ended while no cycle
watched it is finished as it was, its time then counting the time without a cycle. A restart
of the broker (an upgrade, say) thus neither kills jobs nor loses what they taught.

A shadow broker starts no cycles: it reads the state files only, so it can point `state_dir`
at a live instance's.

### Slot states

| State       | Meaning                                                               |
| ----------- | --------------------------------------------------------------------- |
| `ready`     | reset, no runner, takes an assignment                                 |
| `pending`   | an assignment is written but the cycle has not taken it yet           |
| `assigned`  | the cycle took the assignment; the runner waits for its job           |
| `running`   | the runner runs its job                                               |
| `resetting` | the container is being restored from its snapshot                     |
| `failed`    | a reset or a runner start failed; the cycle backs off and tries again |
| `stopped`   | the cycle is not running                                              |
| `paused`    | reset and held out of service until it is resumed                     |

A slot is free only when it is `ready` with no assignment and no kept assignment of the broker
names it.

## Slot protocol (SSH)

The broker runs `ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -i <key> -p <port>
<user>@<address> <command>`. The host's `authorized_keys` forces the command, for example:

```text
restrict,from="192.0.2.10",command="sudo -n /usr/local/bin/forgejo-berth slot -- \"$SSH_ORIGINAL_COMMAND\"" ssh-ed25519 AAAA…
```

`forgejo-berth slot` takes the whole requested command as one argument (or from
`SSH_ORIGINAL_COMMAND`) and knows five commands. Anything else exits 1 with a usage line. The
argument follows `--`, which ends the command's own flags: a request beginning with `-` is then
a request (refused), never a flag such as `--settings-dir`.

`status` prints JSON on standard output. Tokens are never part of it.

```json
{"slots": [{"container": "ci-a", "state": "ready", "state_since": 1763700000,
  "assignment": "",
  "last": {"assignment": "ci-1-ci-a-1763699000-42", "task": 8480, "peak_mib": 2345,
           "seconds": 312.5, "cpu_cores": 1.85, "cpu_pressure": 0.12}}]}
```

`cpu_cores` and `cpu_pressure` are optional: a slot side that does not measure CPU omits them,
and the broker records no CPU figures for that run. `unused` is true for a runner that took no
job, and omitted otherwise.

`assign <container>` reads one assignment (JSON, at most 16 KiB) on standard input, checks it,
and writes it for the container's cycle (mode 0600). It prints the assignment id. It refuses
(exit 1, the reason on standard error) a container that is not a slot, a slot not `ready`, a slot
with an assignment already waiting, and an assignment out of shape.

```json
{"assignment": "ci-1-ci-a-1763700000-42", "uuid": "0f0e0d0c-0b0a-4908-8706-050403020100",
 "token": "<runner token>", "labels": ["ubuntu-latest"], "class": "trusted",
 "unused_minutes": 15}
```

| Field            | Check                                                        |
| ---------------- | ------------------------------------------------------------ |
| `assignment`     | `[a-z0-9][a-z0-9-]{0,127}`                                   |
| `uuid`           | a lowercase UUID                                             |
| `token`          | `[0-9A-Za-z_-]{20,256}`                                      |
| `class`          | `trusted` or `shared`, and configured on the slot            |
| `labels`         | not empty, every name in the class's label table of the slot |
| `unused_minutes` | 1 to 1440                                                    |

`release <container> <assignment>` asks the cycle to stop the assignment's runner if it still
has no job. It refuses a slot that does not hold that assignment.

`pause <container>` and `pause <container> stop` pause the slot (see
[Pausing a slot](#pausing-a-slot)), `resume <container>` resumes it; each prints the slot's
state. A paused slot's `status` entry has `"paused": true` (omitted otherwise), and `assign`
refuses it. A broker of an older version reads neither: it sees a slot that is not `ready`.
The broker's key may pause and resume the host's slots, which takes nothing from it that it
could not deny by assigning nothing; `forgejo-berth pause` uses that.

The host maps label *names* to runner labels by the class's own table, so a shared runner can
never get a host label, whatever the broker sends. The broker sends names only.

## Reset cycle

`forgejo-berth cycle <container>` runs as root, one per container (a systemd template unit). Its
loop:

1. **reset**: drop any assignment not yet taken (its runner was deleted), stop the container,
   restore the snapshot, start it, wait for systemd in it to be `running` or `degraded`;
2. **ready**: wait for an assignment file; parse it as `slot assign` did. One out of shape is
   dropped. After `idle_hours` the container is refreshed (back to 1);
3. **assigned**: attach the class's cache volume, write the runner's configuration (a template
   with `@@RUNNER_UUID@@` and `@@RUNNER_LABELS@@`) and its token into the container, start the
   runner's service;
4. **running**: the runner took its job (a step's process, a host-mode job directory or a
   populated Docker scope under the runner's cgroup). A runner with no job after the
   assignment's `unused_minutes`, or released by the broker, is stopped, unless its journal shows
   it took a task (a job still pulling its image). A job longer than `job_hours` is cut;
5. **finish**: note the task id (the first `task N repo is` line of the runner's journal) and the
   container cgroup's `memory.peak` plus `memory.swap.peak` (rounded up to MiB) in the state's
   `last`, with the job's CPU use (see [CPU](#cpu)); then back to 1. The peak is the job's
   own: the container was started for it alone.

Failures back off from 30 seconds, doubling to 10 minutes. A SIGTERM ends the loop after the
step in progress and leaves the container as it is (`stopped`).

The state goes to `<state_dir>/<container>.json`; the assignment to `<container>.assignment`,
the release to `<container>.release`, a pause to `<container>.pause`.

### Pausing a slot

A pause is the file `<container>.pause` beside the slot's state (written by `pause`, 0644, its
content `finish` or `stop`; removed by `resume`). The cycle reads it at each of its looks:

- **ready**: an assignment written before the pause is still taken and served (the broker
  holds its runner); otherwise the cycle holds the slot (`paused`) at once, without a reset;
- **assigned** (a runner without a job): the runner is stopped as for a release, unless its
  journal shows it took a task; that is no failure of the slot (no back-off);
- **running**: the job runs to its end; with `stop` it is cut as a job over `job_hours` is,
  and the reset stops it;
- **reset**: after every reset the cycle holds a paused slot instead of waiting for an
  assignment.

Held, the slot is `paused`: its container is as the reset left it (an Incus container
restored and running; a Docker slot without containers), and the cycle does nothing but look
for the resume. Resumed, it resets the container (an Incus container is restored from its
snapshot, which the one who paused it may have made anew) and is `ready`.

The state file notes that the cycle holds the slot (`held`). A cycle that starts while the slot
is held and still paused (a broker or a cycle restarted) holds it again at once, without the
reset that a start does otherwise: the container may be in the middle of its change. A slot
paused while no cycle ran is reset at the start, then held. The pause file lives as long as
`state_dir`: the broker's (beside the history) is on disk; an SSH host's default
(`/run/forgejo-berth`) is not kept over the host's reboot.

### Cache volumes

A class's cache volume (`cache_volumes`) is attached to the slot for every job of that class
and kept over the reset. A name with `{host}` and `{slot}` is expanded per slot (the slot side
expands it, on the host for the SSH provider with the host's short name), so that each slot has
a volume of its own: two jobs at once never write the same cache, and a slot's next job finds
what the last one left. A plain name stays one volume for all slots, as before.

A volume that is not there is made at the first use, rather than refused: with one volume per
slot and class, making them all by hand is the error-prone step, and an empty cache volume is
what a new one would be anyway. Docker: `GET /volumes/<name>`, else `POST /volumes/create` with
the labels `org.forgejo-berth.host` and `org.forgejo-berth.cache` (a volume the mount made
would have none). Incus: `GET /1.0/storage-pools/<pool>/volumes/custom/<name>`, else a `POST`
of a custom file system volume in `cache_pool` (the `incus` command: `storage volume show`,
`create`); two cycles that make the same volume at once both go on. berth removes no cache
volume.

## Docker provider

A host's `address` is its engine: `unix:///path`, or `tcp://host:port` with TLS
(`provider.docker.tls`: the CA, the broker's client certificate and key). The client speaks the
Engine API with `net/http` (no SDK), without a version in the paths: an engine answers those
with its own version's shapes, and the fields used are the same since API 1.24, Podman's
Docker-compatible API included. A host also names its `slots`; slot `a` of host `ci-1` is
`<runner_prefix>ci-1-a`.

The reset cycle's steps, for a Docker slot:

1. **reset**: remove the slot's containers (with their anonymous volumes) and its volume; pull
   the runner's image (and the Docker image) when the engine does not have them;
2. **assigned**: with `job_engine: dind`, make the volume `<name>-run` and the slot's own
   Docker `<name>-docker` (`dind_image`, privileged, `dockerd` on a socket in that volume, no
   TCP listener), wait until it answers, and open the socket to the runner's user; then make
   the runner's container from `image`: an init, a small `sh` wrapper, and
   `forgejo-runner one-job --wait` with the assigned UUID, labels and `--token-url` of a file;
   copy the token (and the configuration from `config_template`, if any) into it, owned by
   the image's user, and start it;
3. **running**: the runner's log shows its task line. The wrapper forwards a stop signal to the
   runner, and when the runner exits writes a file and keeps the container up;
4. **finish**: the task from the log, and from the runner's container and the slot's Docker
   (whose cgroup holds the job's containers) their cgroups' `memory.peak` plus
   `memory.swap.peak`, summed, and their `cpu.stat` and `cpu.pressure`, read by `exec` in each
   container (cgroup v2 shows a container its own cgroup). Where there is no `memory.peak`
   (cgroup v1), the stats API's `max_usage`. The sum of the two peaks is a little above the
   peak of their sum: a margin on the safe side.

A job short enough to start and end between two looks of the cycle is still learned: the
runner's log shows its task (its CPU use is then not known).

`job_engine` is what the job's containers run on:

- `dind` (the default): the slot's own Docker, made for the job and removed after it. The job
  can do anything to that engine and nothing to the host's other containers. Docker in Docker
  needs a privileged container; on a rootless engine that is the engine user's privilege
  alone. A rootless Docker inside (`docker:dind-rootless`) needs more subordinate ids than a
  rootless engine has to give, so the rootful Docker image is the default;
- `none`: no engine; the labels must run on the host (`label:host`), in the runner's container;
- `host-socket`: the engine's own socket (`host_socket`) in the runner's container. Every job
  then controls that engine: every container on it, the other slots and their tokens, and on a
  rootful engine the host. Their containers are the engine's, not the slot's: their memory is
  not part of the job's peak, and nothing removes what a job leaves. An explicit opt-in, for
  trusted jobs only.

## Incus provider

The reset cycle's Incus logic (the boot wait and its diagnosis, the cache volume, the
runner's files, the journal's task line) drives an `Instance`, the container as that logic
reads and changes it. Two implement it:

| Operation    | `incus` command, on the host (SSH)               | Incus API, from the broker (`incus`)                       |
| ------------ | ------------------------------------------------ | ---------------------------------------------------------- |
| state        | `incus query /1.0/instances/<c>/state`           | `GET /1.0/instances/<c>/state`                             |
| stop, start  | `incus stop --force`, `incus start`              | `PUT …/state` (`action`, `force`), an operation waited for |
| restore      | `incus snapshot restore`                         | `PUT /1.0/instances/<c>` with `restore`                    |
| exec         | `incus exec <c> --`                              | `POST …/exec`, `record-output`; the records read, removed  |
| write a file | `sh -c 'cat > tmp…'` from standard input         | `POST …/files` (root, 0600), then `chgrp`, `chmod`, `mv`   |
| cache device | `incus config device remove/add`                 | `GET` the instance, `PUT` it back with `If-Match`          |
| its log      | `incus info --show-log`                          | `GET …/logs/lxc.log`                                       |
| the runner   | its cgroup and `/proc/<pid>/root`, from the host | one `exec` of a script reading the same from inside        |
| peak         | the cgroup's `memory.peak`, `memory.swap.peak`   | the state's `memory.usage_peak`, `swap_usage_peak`         |
| CPU          | the cgroup's `cpu.stat`, `cpu.pressure`          | the same files, read by `exec` from inside                 |

After each runner the cycle saves the runner's journal of this boot (`journalctl --boot
--unit=<runner_service>`, its last `journal_lines`): an SSH host's cycle prints it to its own
output (the unit's journal), the incus provider's into the broker's log, a record per line at
info. When the runner failed (its service did not start, or it exited within a minute without
a job), the lines are at warn: the journal is the only account of why, and the next reset
loses it. The docker provider logs the runner container's output as one record, at debug, at
warn for a failed runner.

Inside a container, cgroup v2 with its cgroup namespace shows the container's own cgroup at
`/sys/fs/cgroup`, the tree the host sees under `lxc.payload.<c>`. The runner is looked at once
for both questions of a look (active, busy). An answer that does not come (the API away for a
moment) counts as active, not busy: the next look asks again, rather than ending a job that
runs. Where the state reports no peak (a kernel or server without `memory.peak`), the
container's own `memory.peak` is read from inside.

A host's `address` is `https://host:port`, with the broker's client certificate and the
server's certificate pinned (`tls.ca_file`: a certificate that is no CA must be the server's
exactly; a CA checks the chain and the name), or `unix:///path`, the local socket. The
broker's status of a host is a `GET /1.0` that must say `auth: trusted`. The client uses
`net/http` only; the response shapes are those of the Incus REST API (sync, async with an
operation waited for at `/1.0/operations/<id>/wait`, error).

## Configuration

One YAML file, `--config` (default `/etc/forgejo-berth/config.yml`). Secrets are files: the
admin token, each trusted owner's token, the SSH key. A relative secret path is looked up in
`$CREDENTIALS_DIRECTORY` when set (systemd `LoadCredential`), else in the working directory.
The command line takes no secret. The full reference is in the README.

With the `docker` and `incus` providers, the slots' settings are the provider's, the same on
every host, but for three a host may set for itself: `tls` (a server with a certificate of its
own to pin, or another CA; each field it sets replaces the provider's, the others are the
provider's), `config_template` (the runner's configuration on that host) and `labels` (per
class: a class the host lists has the host's table, the others the provider's). A configuration
without them reads as before. An SSH host's slots have these in their slot files, on the host.

```yaml
forgejo:
  url: https://forgejo.example.org
  admin_token_file: admin.token        # write:admin, read:repository
trusted:
  owners:
    alice: alice.token                 # write:user of alice
  labels: [ubuntu-latest, docker]
shared:
  labels: [docker]
hosts:
  - name: ci-1
    address: ci-1.example.org
    memory_mib: 14000
    features: [gpu]
provider:
  type: ssh
  ssh: {user: berth, port: 22, key_file: ssh_key,
        known_hosts_file: /etc/forgejo-berth/known_hosts}
```

## Who may get which runner

`offer(job)`:

- a job of a trusted owner gets a runner at that owner's user scope
  (`POST /api/v1/user/actions/runners` with the owner's token), with the labels it asks for, if
  each is a trusted label or a host feature; otherwise none;
- anyone else's job gets a global runner (`POST /api/v1/admin/actions/runners`) if each label it
  asks for is a shared label; otherwise none;
- a job asking for no label gets none.

Jobs no runner may take are counted (`unservable`) and left to other runners.

A job asking for a feature goes only to hosts that have it.

## Learning

The history keeps every learned run: repository, job name, family (the name without a trailing
matrix part: `build (32)` is of family `build`), time, peak MiB, reserved MiB (0 for a host
alone), seconds, host.

**Estimate.** For a job, the largest peak among its last `history_runs` (5) runs and its runs
within `spike_days` (30), whichever reach further back. A rare spike thus counts for a month,
and a pattern that changed fades after five runs. Looked up for the job itself, then its family,
then its repository; the first that has runs wins.

**Prediction.** The estimate times `margin` (1.2) for the job's own history, times
`relative_margin` (1.5) for a family's or repository's, at least `floor_mib` (512). A job
without any history gets `unknown_mib` (6144): enough for a typical container build, while a
host of 12 GiB or more still runs something beside it; its first run teaches the history. With
`unknown_mib: 0` such a job gets a host alone.

**Learned ends.** Only `success` and `failure`: a cancelled job stopped early, its peak says
little. A job whose peak exceeded what was reserved is learned at once and counted
(`underestimated`): many of them mean the margins are too small. A job that ran without a task
line, or whose run cannot be read, is counted `unlearned`.

**Retention.** Runs older than `retention_days` (180) are deleted, once a day.

## CPU

Admitting by memory alone lets two CPU-heavy jobs share a host, and each then runs about twice
as long. A browser end-to-end suite that takes 10 minutes alone can take over 20 beside another
such suite and hit its job timeout, while both fit in memory easily.

**Recording** (always on). When the cycle first sees the job running, it reads the container
cgroup's `cpu.stat` `usage_usec` and `cpu.pressure` `some total`; after the job it reads them
again. The differences over the job's run time give:

- `cpu_cores`: the average cores the job used (CPU time over wall time). The boot and the wait
  before the job are left out;
- `cpu_pressure`: the share of the job's run time during which some of its tasks waited for a
  CPU (0 to 1; 0 where the kernel has no pressure accounting). A high value says the job was
  slowed by its neighbours or by its container's CPU limit.

Both go into the run's history row (`cpu_cores`, `cpu_pressure`, NULL when not measured), and
into the log line of the learned job.

**Prediction.** By the memory's rules (the largest of the last runs or of the spike window, job,
family, repository), from the runs that have CPU figures, without a margin: the overcommit
factor below is the margin. A job without any gets `cpu.unknown_cores` (1).

**Admission** (optional). Either or both of:

- `cpu.overcommit` above 0 (off by default): a job starts on a busy host only while the
  predicted cores of the host's jobs, its own included, stay within the host's `cpu_cores`
  times the factor. An average is below a job's busiest moments, so a factor of 1.5 to 2 still
  keeps two heavy jobs apart while letting light ones pack. An idle host takes any job, as with
  memory: a job never waits for cores no host has;
- `hosts[].max_jobs`: at most that many jobs on the host at once, whatever they need.

The predicted cores are kept with the assignment (`reserve_cores`), as its memory is. Watching
`cpu_pressure` of the learned jobs shows whether the rule is needed and which factor fits.

## Scheduling

### Order

Jobs are taken in the order the broker first saw them waiting (longest waiting first, then by
id). Forgejo does not say since when a job waits; a job run again arrives anew.

### Claims and reservations

Which job a runner gets is Forgejo's choice. When a runner asks for a task, Forgejo lists the
waiting jobs the runner's scope can see, by run priority (highest first), then by when the job
was last updated and its id (oldest first), and hands the runner the first whose `runs-on`
labels are all among the runner's labels (`GetAvailableJobsForRunner` in
`models/actions/task.go` and `CreateTaskForRunner` in `services/actions/task.go` of Forgejo).
The scope decides what a runner sees:

- an owner's runner: the jobs of that owner's repositories;
- a global runner: the jobs of every repository, an owner's too.

A runner **covers** a job when the job is in its scope and its labels cover the job's. A global
runner thus covers a trusted owner's job that asks for shared labels only, and Forgejo may hand
it that job before the owner's own runner asks. It covers a job no runner is offered for too,
as long as its labels match: Forgejo knows nothing of berth's offers. berth takes the oldest
first by its own [order](#order); it cannot see a run's priority.

Which runner asks first is not known: slots boot at different speeds, and the runners of one
pass start together. So:

- the runners on their way (assigned, not yet running) **claim** jobs in the order they were
  registered: each claims the oldest unclaimed waiting job it covers, and no new runner is
  started for it. That order is a guess; it decides only how many runners are started. When it
  is wrong, a runner whose job another took is released (see
  [Release and loss](#release-and-loss)), and a job left without one gets a runner at the next
  look;
- a new runner for a job reserves the most that this job and every older waiting job it covers
  need, claimed or not: if it asks before the runners that claimed them, it gets the oldest. It
  claims the oldest unclaimed one. It does not reserve for younger jobs: they go to the runners
  after it. Reserving for every job a runner could take would keep a host to one job at a time
  while a large job waits;
- a job whose reservation, or one of those older jobs', is unknown (`unknown_mib: 0`) gets a
  host alone;
- a runner's candidates are all the waiting jobs it covers, of any owner for a global runner:
  the job it ran is looked up among them, and it is released once none of them waits.

A runner still gets a younger job than it reserved for when the runners started for younger
jobs ask first and take its job and the older ones; berth does not reserve for that, for the
reason above. A run priority set in Forgejo can also put a job ahead of older ones.

```mermaid
sequenceDiagram
  participant B as berth
  participant O as alice's runner
  participant G as global runner
  participant F as Forgejo
  Note over F: waiting: alice/app test (docker, older), carol/site lint (docker)
  B->>O: start for alice's job: reserves for it
  B->>G: start for carol's job: reserves for both, candidates both
  G->>F: asks first
  F-->>G: alice's job (oldest it covers)
  O->>F: asks
  F-->>O: nothing of alice's waits: released, unused
  Note over B: next look: carol's job unclaimed, a new global runner
```

### Placement

For each unclaimed job with an offer: the ready slots on hosts with its features, not held for
another job. A job expected to need a host's whole memory or more gets the host alone (it may
still fit with the margin and swap; it is never shut out), and only on an idle host. Otherwise it
needs `need <= memory_mib - reserved`, and within the [CPU](#cpu) rules when they are on.
Among the fitting slots the host with the most memory free wins, then the less busy one.
Nothing starts beside a job that has a host alone.

### Starvation

A job that does not fit lets younger, smaller jobs go ahead until it has waited
`starve_minutes` (10). Then it holds the fitting host with the most memory free: nothing new
starts there until the job fits.

### Back-off

Forgejo may list a job as waiting and still hand it to no runner: a run held by its concurrency
group behind an earlier run that never ends, for example. The API berth reads shows neither the
group nor the reason. Without a rule, berth would start a runner for the job, the runner would
wait `unused_minutes` for nothing, and the next look would start the next one: a slot and a
reservation taken for as long as the job waits, for hours.

berth tells such a job from its own history. A runner that went unused counts against every job
it covered (its candidates) that still waits: Forgejo handed none of them to a runner that asked
for `unused_minutes`. A runner stopped sooner (released, its slot paused) proves nothing and does
not count, nor does a lost one. After `backoff.unused_runs` (3) such runners in a row the job is
**backed off** for `backoff.base_minutes` (default `unused_minutes`); each runner gone unused
after it is tried again doubles that, up to `backoff.max_minutes` (240): 15, 30, 60, 120, then
240 minutes with the defaults. `unused_runs: 0` turns the back-off off.

While backed off, a job:

- gets no runner, and no runner on its way claims it: such a runner claims the next job it
  covers instead;
- is reserved for by no new runner, and does not hold a host when it starves;
- keeps no runner from its release: a runner whose other jobs went is released as usual;
- stays a candidate of the runners that cover it, an owner's job under a global runner too (see
  [Claims](#claims-and-reservations)). A back-off belongs to the job's id, whichever scope's
  runner would take it: if Forgejo hands the job out after all, it is found among the
  candidates, and the back-off ends.

When the back-off ends, the job is tried again as if it arrived then: its place in the order and
its wait for the starvation count from that moment. The count goes back to 0 when a runner
takes the job (whatever its end), and the back-off is forgotten when the job waits no more (it
ran, was cancelled, its run deleted); a job run again arrives anew.

The log has one line for each step: `backed off` (warn: the job, its run, the count, the
minutes and until when), `back-off over; the job is tried again`, `a backed-off job ran;
back-off reset`, `a backed-off job waits no more; back-off forgotten`.

The back-offs are kept in the history (the `backoffs` table, by job id), written at each change
and read at start, as the kept assignments are: a restart neither forgets a back-off nor starts
the churn anew, and those of jobs that went meanwhile are forgotten at the first look. The state
file would not do: it is an output, optional, and not read back.

Runners that cannot reach Forgejo at all go unused too, and back every job off after a while:
the back-off then slows the churn of a broken setup as well. `unused` rising without `learned`
shows that case.

### Release and loss

- An assigned runner none of whose candidate jobs waits any more (cancelled, deleted, taken by
  another runner) or all of whose waiting ones are backed off is released once it is 60 seconds
  old and seen so at two looks in a row.
- The slot's cycle stops a runner unused for `unused_minutes` (15) anyway.
- An assignment no slot reports for 90 seconds is lost (its cycle restarted before taking it);
  its runner is deleted. One on a host no longer configured is lost at once.

## Observability

- `GET /metrics`: Prometheus text format, hand-written (no client library):

  | Metric                            | Labels   | Kind    |
  | --------------------------------- | -------- | ------- |
  | `forgejo_berth_last_tick_seconds` |          | gauge   |
  | `forgejo_berth_waiting_jobs`      | `kind`   | gauge   |
  | `forgejo_berth_host_reachable`    | `host`   | gauge   |
  | `forgejo_berth_reserved_mib`      | `host`   | gauge   |
  | `forgejo_berth_events_total`      | `event`  | counter |
  | `forgejo_berth_estimates`         | `source` | gauge   |
  | `forgejo_berth_jobs_learned`      |          | gauge   |
  | `forgejo_berth_backed_off_jobs`   |          | gauge   |
  | `forgejo_berth_shadow`            |          | gauge   |

  Events: `assigned`, `learned`, `unlearned`, `unused`, `failed`, `underestimated`, `released`,
  `backed_off` (each back-off, the first and each longer one), and in shadow mode
  `would_assign`.
- The reset cycles' metrics, one series per slot:

  | Metric                                   | Labels                         | Kind    |
  | ---------------------------------------- | ------------------------------ | ------- |
  | `forgejo_berth_cycle_state`              | (`host`), `container`, `state` | gauge   |
  | `forgejo_berth_cycle_jobs_total`         | (`host`), `container`          | counter |
  | `forgejo_berth_cycle_failures`           | (`host`), `container`          | gauge   |
  | `forgejo_berth_cycle_last_reset_seconds` | (`host`), `container`          | gauge   |

  An SSH host's cycle writes them to `<textfile_dir>/forgejo_berth_cycle_<container>.prom`
  for node_exporter, without `host` (node_exporter's own `instance` is the host). The cycles the
  broker runs (`docker`, `incus`) have them in the broker's `/metrics` and `metrics_file`, read
  from their state files at every look, with the `host` label (a slot's name is the same on
  several hosts). `jobs_total` counts since the cycle started on an SSH host; a broker's cycle
  resumes its count from the state file. A shadow broker runs no cycles and has none.
- `GET /state`: JSON (`last_tick`, `waiting`, `unservable`, `hosts` with `reachable` and
  `reserved_mib`, `assignments` without tokens, `counts`, `estimates`, `jobs_learned`,
  `backoffs` (each job counted: `job_id`, `run_id`, `repo`, `name`, `unused`, `backed_off`,
  `until`), and `cycles`, the broker's own cycles as their metrics show them). The same
  document can be written to `state_file` atomically, for tools that read files, and the
  metrics to `metrics_file` (a node_exporter textfile collector's `.prom` file).
- `GET /healthz`: 200 while the last look is younger than three intervals.
- Logs: `log/slog`, text or JSON (`log.format`), one line per decision.

## Security

- The admin token and the owners' tokens are read from files at start and never logged.
- A runner's token exists in the broker's memory from registration until `Assign` returned; it
  is not kept in the history (the kept assignment has no token), not in the state, not in logs.
  On the host it lives in the assignment file (0600, root) until the cycle takes it, then in the
  container until the next reset. With a provider whose slot side is the broker's, the
  assignment file is in the broker's `state_dir` (0600, the broker's user) for the seconds
  until the slot's cycle takes it.
- A Docker slot's runner reads its token from a file of its own container (0400, the runner's
  user); the job engine's privileges are in [Docker provider](#docker-provider).
- SSH: `StrictHostKeyChecking=yes`, `BatchMode=yes`, the key from the configuration with
  `IdentitiesOnly=yes`; the host side forces one command and parses its arguments strictly.
- A runner name has a configured prefix (`runner_prefix`, `berth-`): the sweep deletes runners
  of that prefix only.

## Shadow mode

`forgejo-berth run --shadow` (or `shadow: true`) runs every look but acts on nothing: it
registers, assigns, releases and deletes no runner, sweeps nothing and records no run. It logs
each placement it would make (`would assign`) and counts them. It computes the
[back-offs](#back-off) itself, in memory, from the runners it sees settled (the live
instance's, each counted once however often it sees it), logs `would back off` and leaves
backed-off jobs out of its placements; it neither reads nor writes the live instance's.

The history is opened read-only in shadow mode, and the kept assignments are read from it at
every look. A shadow instance can therefore point `history.path` at the database of a live
instance on the same host (SQLite allows concurrent readers): it predicts from the live history
and sees the live reservations, so its decisions can be compared with the live log before the
shadow instance takes over. The database schema is part of the interface:

```sql
CREATE TABLE runs (repo TEXT NOT NULL, name TEXT NOT NULL, family TEXT NOT NULL,
  at REAL NOT NULL, peak_mib INTEGER NOT NULL, reserved_mib INTEGER NOT NULL,
  seconds REAL NOT NULL, host TEXT NOT NULL, cpu_cores REAL, cpu_pressure REAL);
CREATE TABLE assignments (id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE backoffs (job INTEGER PRIMARY KEY, data TEXT NOT NULL);
```

`at` is Unix seconds. `data` is the assignment as JSON: `id`, `host`, `container`, `scope`,
`runner_id`, `labels`, `candidates` (jobs: `id`, `run_id`, `repo_id`, `owner`, `repo`, `name`,
`runs_on`), `reserve_mib`, `exclusive`, `since`, and `reserve_cores` when the CPU admission is
on. A `backoffs` row's `data` is a job's back-off as JSON: `job` (as a candidate), `unused`,
`until` (Unix seconds, 0 before the first back-off), `last` (the assignment last counted) and
`retried`. A database without the CPU columns or the `backoffs` table gets them when berth opens
it for writing; read-only, it is used without CPU figures.

Taking over: stop the live instance, start berth on its history without `--shadow`. The kept
assignments carry over (set `runner_prefix` to the old instance's prefix so the sweep spares
nothing it should delete and deletes its leftovers).

## Forgejo API used

| Call                                                      | Token        | Purpose             |
| --------------------------------------------------------- | ------------ | ------------------- |
| `GET /api/v1/admin/runners/jobs`                          | admin        | waiting jobs        |
| `GET /api/v1/repositories/{id}`                           | admin        | owner and full name |
| `POST /api/v1/admin/actions/runners`                      | admin        | global runner       |
| `POST /api/v1/user/actions/runners`                       | owner        | owner's runner      |
| `GET`, `DELETE` the same paths (`/{id}`)                  | admin, owner | sweep, delete       |
| `GET /api/v1/repos/{owner}/{repo}/actions/runs/{id}/jobs` | admin        | task id, conclusion |

The registration body is `{"name": …, "ephemeral": true}`; the answer has `id`, `uuid` and
`token`. An owner's token needs `write:user`; the admin token `write:admin` and
`read:repository`. Lists are paged (`page`, `limit=50`, at most 40 pages), with or without a
wrapping object (`jobs`, `runners`). A 404 on delete is success (Forgejo removed the ephemeral
runner after its job). Tested against Forgejo 17 and forgejo-runner 13.

## Testing

- The scheduler, the estimate, the broker's look and the back-off are tested table-driven, with
  a fake forge and a fake provider, on a fake clock.
- The Forgejo client is tested against an `httptest` server that serves the API's shapes.
- An end-to-end test runs the broker against a fake Forgejo (`httptest`) and an in-memory
  provider that plays the slot's life: ready, assigned, running, finished with a peak, learned,
  and the next run packed by it.
- The slot command and the cycle are tested on a temporary directory with a fake system; the
  slot side run in the broker and the cycle's resume too.
- The incus provider is tested against a fake Incus API (HTTPS with client certificates,
  built from the REST API's documented shapes): a slot's whole life, failed operations, the
  pinned server certificate (a host's own too), the cache volumes made at their first use, the
  runner's journal in the log; and end to end by `scripts/real-forgejo.sh incus`, below.
- The docker provider is tested against a fake Engine API on a unix socket, and end to end by
  `scripts/real-forgejo.sh docker`: a real Forgejo, the provider on the engine the test runs
  on, real jobs in each slot's own Docker, learned and packed by their peaks; each slot with a
  cache volume of its own, made at its first use, and one slot paused throughout, which runs
  no job and is ready once resumed.

### The incus end-to-end run

`scripts/real-forgejo.sh incus` runs the scenario through the incus provider against a real
Incus, with real Forgejo jobs:

```mermaid
flowchart LR
  test["go test (the broker)"] -- "HTTPS :8443, client certificate" --> incusd
  test -- "unix socket (a ping)" --> incusd
  subgraph host["privileged Docker container: the Incus host"]
    incusd["Incus"] --> a["container a"]
    incusd --> b["container b"]
  end
  a -- "forgejo-runner one-job" --> forgejo["Forgejo (Docker)"]
  b -- "forgejo-runner one-job" --> forgejo
```

- **Set up** (`e2e/real/incus/`): a Debian container (`INCUS_HOST_IMAGE`) installs Incus,
  initialises it (`dir` storage, a bridge) and listens on `:8443`, published on
  `127.0.0.1:INCUS_PORT`. Slots `a` and `b` are Debian 13 containers with forgejo-runner (the
  runner image's binary) as the systemd service `forgejo-runner`, run as `runner`, and a
  snapshot `clean` taken after the setup, in the project `INCUS_PROJECT` (`default`; another
  is made with the default project's profiles and images). The broker's client certificate is
  trusted restricted to that project; the server's own certificate is pinned (`tls.ca_file`),
  though the test reaches it at an address its certificate does not name.
- **Checked**: the jobs pass; the builds' peaks are learned and not 0, and their CPU use; the
  second build reserves its learned peak; each slot's cache volumes (`{host}-{slot}`) are made
  at their first use and keep a build's file across the slot's resets; `forgejo-berth pause`
  holds slot `b` out of service after the first round (it runs nothing more, `paused` in the
  broker's metrics with the cycles' other series) and `forgejo-berth resume` makes it ready
  again; the runners' journals are in the broker's log at info; the local socket answers.
- **Needs** an engine that grants `--privileged` with cgroup v2 and the `cpu`, `memory` and
  `pids` controllers delegated (rootless Docker with systemd's delegation works), and
  `images:debian/13` from the network. No KVM: containers only, no virtual machine slots.

What the harness changes from a production host, and why:

- The slot containers are privileged (`security.privileged`): a rootless engine's container has
  65536 ids of its own, too few to give an unprivileged Incus container a map of its own.
- `raw.lxc: lxc.apparmor.profile=unchanged`: where the engine confines its containers with
  AppArmor (as rootless Docker's rootlesskit is), LXC cannot switch to a profile of its own
  (`Already AppArmor confined, but new label requested`), and keeps the one it is in.
- Memory accounting: cgroup v2 enables a controller for a cgroup's children only while the
  cgroup holds no processes. LXC puts a container beside PID 1's cgroup (a trailing
  `init.scope` pruned), so the host's entrypoint moves itself to `init.scope`, Incus to a leaf
  of its own, and enables the controllers in the emptied root. Without that a container has no
  memory controller, no `memory.peak`, and every peak would be 0.
- Incus 6.0 (the LTS) reports no `memory.usage_peak` on cgroup v2: the peaks come from the
  container's own `memory.peak`, read from inside (the provider's fallback). The state's peak
  is used where a server reports it.
- The containers resolve Forgejo through `/etc/hosts` (written before the snapshot): Incus'
  DNS does not know the Docker network's names.
- LXC logs `Failed to load bpf program` (no device cgroup filter in a nested container); it is
  not fatal.
