# The Programs You Actually Run

Everything under `cmd/` is a separate program you can start from a
terminal. Each section below is one of them.

## `collector/`

The worker that actually goes and fetches flight prices. Runs two ways:

- By default: run it once by hand for a specific route/date, useful for
  testing. It prints what it found and saves it.
- With `-worker`: instead runs forever in the background, watching for
  search jobs that other parts of the system create, and works through
  them — a few at a time, not all at once, so it doesn't hammer Google.

Files:
- **`main.go`** — reads the command-line options and picks which of the
  two modes above to run.
- **`worker.go`** — the background-worker loop: keeps checking for a new
  job, grabs one, and works on it without sitting idle waiting for it to
  finish — it can be checking for the *next* job at the same time. If a
  worker crashes partway through a job, another worker (or the same one,
  after restarting) will notice later and pick it back up.
- **`activities.go`** — the actual "go check flight prices" step one job
  runs: calls the search logic and saves a short summary of the result.

## `email-intake/`

Stands in for reading a traveler's actual email until real email support
is built. Also runs the background loop that pushes each traveler's
request forward, step by step — asking the "brain"
(`internal/agents`) what to do next, starting new searches, and sending
the final answer once it's ready.

- **`main.go`** — three modes: `-worker` (keep checking every open
  request and nudge it forward, one step at a time), `-start` (pretend a
  new request email just arrived — creates one from command-line
  options you type in), `-signal` (pretend a follow-up email arrived —
  adds a new note or requirement to a request that's already in
  progress).

## `routesearch/`

Run the full money-saving flight search once, right now, from your
terminal — ask a question, get an answer, done. No database polling, no
background worker involved. This is the easiest way to try the search
yourself today (see the root `README.md`'s "Try it now").

- **`main.go`** — reads your command-line options, runs the right kind
  of search (plain one-way, round trip, or flexible dates), and prints a
  readable summary of what it found.

## `search-api/`

Meant to be a fast lookup service: "has anyone already searched this
route recently? Tell me the answer right away, without scraping
anything new." **Not built yet** — running it today just prints a
placeholder message.

- **`main.go`** — scaffold only.
