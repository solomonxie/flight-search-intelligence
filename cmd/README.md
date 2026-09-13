# The Programs You Actually Run

Everything under `cmd/` is a separate program you can start from a
terminal. Each section below is one of them.

## `agent-worker/`

The "brain" (`internal/agents`) actually running, all the time, in the
background. It sits and waits for a small message saying "this traveler's
request is ready for a decision" — no checking a clock, it only wakes up
because something real happened. When it gets one, it asks the decision
logic what to do, and either kicks off a new search (by sending its own
message onward, for `collector/` to pick up) or decides the request is
done and writes the final answer down. If it decides there's nothing
more to try, it simply sends no further message — that silence is what
makes the loop stop.

- **`main.go`** — the only file, and the whole program: wait for a
  message, act on it, repeat.
- **`Dockerfile`** — container image for the Helm Deployment in `helm/`
  (DESIGN.md "Infra"); build from the repo root, not this directory.

## `collector/`

The worker that actually goes and fetches flight prices. Runs two ways:

- By default: run it once by hand for a specific route/date, useful for
  testing. It prints what it found and saves it.
- With `-worker`: instead runs forever in the background, waiting for
  search jobs `agent-worker/` sends its way, and works through them — a
  few at a time, not all at once, so it doesn't hammer Google. Once a job
  finishes, it tells `agent-worker/` so the traveler's request can move
  forward.

Files:
- **`main.go`** — reads the command-line options and picks which of the
  two modes above to run.
- **`worker.go`** — the background-worker loop: waits for the next "a
  job is ready" message, works on it without blocking the wait for the
  *next* message, and — this is the part worth calling out — only
  considers a message "handled" once the job it triggered is actually
  finished, not the moment it arrives. That way, if a worker crashes
  partway through a job, the message isn't lost — it comes back around
  and gets tried again, by this worker after it restarts or by another
  one.
- **`activities.go`** — the actual "go check flight prices" step one job
  runs: calls the search logic and saves a short summary of the result.
- **`Dockerfile`** — container image; the Helm Deployment runs it in
  `-worker` mode (default `CMD`), not the direct-run CLI mode.

## `email-intake/`

Stands in for reading a traveler's actual email until real email support
is built. Unlike the other two workers, this one does *not* run in the
background — each run does one small thing and exits, the same way one
incoming email would trigger one action, not a persistent process.

- **`main.go`** — two modes: `-start` (pretend a new request email just
  arrived — creates one from command-line options you type in, and sends
  the very first "ready for a decision" message to get the loop going),
  `-signal` (pretend a follow-up email arrived — adds a new note or
  requirement to a request that's already in progress; no message needed
  for this one, since whatever runs next for that request reads the
  updated notes on its own).
- **`Dockerfile`** — container image; the Helm chart runs it as a
  Kubernetes Job, not a Deployment (this binary is one-shot).

## `routesearch/`

Run the full money-saving flight search once, right now, from your
terminal — ask a question, get an answer, done. No database polling, no
background worker involved. This is the easiest way to try the search
yourself today (see the root `README.md`'s "Try it now"). Exhaustive by
default (no query cap — see root `README.md` "Why this beats a plain
flight search"), so before it spends a single real scrape it estimates
how many it might take and asks you to confirm; `-yes` skips that
prompt, `-budget N` caps the run instead of running exhaustively.

- **`main.go`** — reads your command-line options, runs the right kind
  of search (plain one-way, round trip, or flexible dates), and prints a
  readable summary of what it found.
- **`confirm.go`** — the pre-search estimate + y/n prompt: how many
  candidate routes, worst-case scrape count, rough time at the current
  pacing.

## `search-api/`

Meant to be a fast lookup service: "has anyone already searched this
route recently? Tell me the answer right away, without scraping
anything new." **Not built yet** — running it today just prints a
placeholder message.

- **`main.go`** — scaffold only.
- **`Dockerfile`** — container image; builds and runs today (it just
  doesn't serve anything real yet, per the scaffold above).
