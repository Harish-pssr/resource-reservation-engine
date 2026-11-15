# Running it

How to run the engine and its console on a development machine, how to run the experiments, and how the benchmark machine was prepared. This is **not** a production deployment guide: the development setup turns fault injection on and publishes the stores' ports (see [limitations.md](limitations.md)).

## What you need

- Go 1.27 (the version in `go.mod`)
- Docker with Compose (PostgreSQL 16 and Redis 7 run in containers from `docker-compose.yml`)
- Windows (PowerShell) or Linux/macOS (bash): every script exists in both forms

## Start everything

```bash
scripts/up.sh          # or scripts\up.ps1 on Windows
```

This starts Postgres and Redis, builds and starts the API with its console, creates a first event, and keeps the server running in the background, restarting it if it exits (so the crash demos can show recovery). When it finishes it prints the console address (http://localhost:8080) and the admin token for the failure and checker pages. The API's log is `logs/api.log`. It rebuilds the server when the code is newer than the running build.

```bash
scripts/down.sh            # stops everything and keeps the data
scripts/down.sh --clean    # also removes the containers and deletes their data
```

To change how the server runs, set variables first, for example `STRATEGY=optimistic ROOM_RATE=200 scripts/up.sh`. The full list is in [architecture.md](architecture.md#configuration).

## Try it in the console

- **Create event**, then **Book seats** (pick seats, proceed, pay) or **Surge** (send a crowd and watch the seat map).
- **Failures** arms a crash at a named point (needs the admin token) and has guided demos: kill the server before the hold reply or before the confirm reply, then retry and see that nothing is duplicated.
- **Checker** reads the database alone and verifies the rules after any activity.

## Tests

```bash
scripts/test.sh                       # go vet and go test ./...  (integration tests need scripts/up first)
scripts/crash-tests.sh                # the real-kill scenarios, 5 repetitions each; raw evidence in benchmark-scratch/
```

The crash tests build the real API binary, arm a hook, drive a request, and check that the process really exited with status 137. In evidence mode a skipped run is a failure.

## Run the experiments

```bash
scripts/bench-e1.sh -runs 5 -out docs/data/<date>-<env>/e1      # oversell test, every strategy
scripts/bench-e2.sh -out ...                                    # capacity and latency per strategy
scripts/bench-e3.sh -strategy locking -out ...                  # surge with and without the waiting room
scripts/bench-e4.sh -out ...                                    # failures under load: kills, restarts, expiry race
python scripts/results-tables.py                                # the tables of benchmarks.md, from the records
```

Each runner starts the API as its own process, resets the database and the cache (the waiting room's keys included), seeds a fresh event, drives the load, collects the metrics, then runs the independent checker and the reconciler. Every flag has a default; run a runner with `-h` to list them. The benchmark refuses to run on battery power, refuses to measure against a server with fault injection enabled, and records its environment in every result. **Numbers from a development laptop are for smoke tests only.**

## The benchmark machine

The published numbers come from one Linux VM (a 16-vCPU `n2-standard-16`-class machine with an SSD disk, Ubuntu 24.04), prepared and checked by the scripts in `scripts/vm/`:

| Script | Purpose |
|---|---|
| `setup.sh` | Installs Docker and Go (the checksum of the Go archive is verified), stops automatic updates and monitoring agents that would take CPU, builds the binaries and records the machine |
| `pinning.env` | The **core plan**: each part on whole physical cores, no two parts sharing a core. On this machine the two threads of a core are CPU *n* and *n*+8, so a contiguous split would have shared cores |
| `pinning-small.env` | The small-backend profile for the surge experiment (PostgreSQL on one core, an API pool of 8) |
| `verify-pinning.sh` | Proves the split from the running processes, and refuses a plan in which two parts share a physical core |
| `run-pinned.sh` | Starts the stores with their cores, runs the API under its cores and the benchmark on its own, then the experiment |
| `dry-run.sh` | Every runner at a tiny size, timed, to catch setup mistakes before real runs |

```bash
scripts/vm/setup.sh
scripts/vm/verify-pinning.sh --plan-only         # check the plan against the machine's topology
scripts/vm/dry-run.sh                            # never quoted
scripts/vm/run-pinned.sh e1 -runs 5 -out docs/data/<date>-linux-vm/e1
BACKEND=small scripts/vm/run-pinned.sh e3 -strategy locking -factors 1.5,2,2.5 -duration 10s -max-rate 8000 -out ...
```

The commit a run came from is recorded in its result: a machine that receives a clean archive of the tree (no repository) carries the commit in a `BUILD_COMMIT` file. Results carry no host name, address or user name.

## Safety notes

- Fault injection is off unless `FAULTS_ENABLED=true`, its controls need `ADMIN_TOKEN`, and `scripts/up` turns it on **for development only**. The throughput experiments refuse to measure against a server with it on; only the failure experiment (E4) enables it, on purpose.
- Do not expose the development compose file's ports (Postgres 55432, Redis 56379) to a network you do not control.
- There is no user authentication; a buyer is a client-chosen string. This is a study of correctness and throughput, not a ticket-selling product.
