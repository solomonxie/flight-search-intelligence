# What Each Package Does

Plain-English tour of the code that isn't a runnable program by itself
(that's `cmd/` — see `cmd/README.md`). Each section is one subfolder.

## `agents/`

This is the "brain" that decides what to search for next, and when to
stop and send the final answer back. Right now it doesn't use a real AI
yet — it uses a simple, fixed rule instead, so the rest of the machinery
around it can be built and tested first (a real AI gets swapped in
later, in one place, without touching anything else here).

The important trick: instead of keeping track of "where we are" in the
computer's memory while it works, it writes down its progress as a row
in the database after every single step. That means if the program
crashes or gets restarted, nothing is lost — it just reads its own notes
and picks up exactly where it left off. And it doesn't sit there
checking "is it my turn yet?" on a timer either — a step only runs
because a small Kafka message said "check this now" (see `kafka/`
below), pushed by whatever just happened (a search finished, an email
arrived).

- **`spec.go`** — the basic "form" every request fills out: where the
  traveler wants to go, when, budget, plus any special requests typed in
  plain English (like "must be there for Christmas"). Also defines what
  one decision looks like, and what gets written down after each step.
- **`task.go`** — describes one search job: exactly what to search for,
  and what comes back (price, how long the trip takes, which airports).
- **`decide.go`** — the (currently fake) decision-maker. What it's
  supposed to do eventually: read the request and decide what to search
  next, or whether what's been found already is good enough. What it
  actually does today: a simple rule — try the search once; if it found
  any flight, call it done; if not, loosen the requirements a bit and try
  again, up to 3 times before giving up.
- **`reconcile.go`** — the two moments this package actually does
  something. `Decide` runs when a request is ready for a decision: asks
  the decision-maker what to do, then either starts a new search job or
  writes the final answer down. `RecordTaskResult` runs right after a
  search job finishes: writes down what it found and hands the request
  back to "ready for a decision" so `Decide` runs again next.

## `catalog/`

The only part of the code allowed to read from or write to the database
(currently a simple SQLite file — a database that's just one file on
disk, no server needed). It's explicitly *not* allowed to create or
change what tables/columns exist — that's a separate, more careful
process (see `databases/README.md`).

- **`catalog.go`** — opens the database, and refuses to start if it
  wasn't set up properly first, instead of failing with a confusing
  error later. Has functions to save a flight price that was found, look
  up the cheapest price seen before for a route, and save a full record
  of one search (what was tried, what was found).
- **`agent.go`** — the two tables the "brain" above depends on: one row
  per traveler request, one row per individual search job. (Making sure
  two workers don't grab the same job at the same time is handled by
  Kafka now, not here — see `kafka/` below.)

## `common/`

Small helpers that don't belong anywhere more specific.

- **`env.go`** — reads a simple settings file (`.env`) so things like API
  keys or file paths can be set once instead of typed out every time.

## `googleflights/`

The part that actually checks flight prices. It visits the public Google
Flights website the same way a person's browser would, and reads the
prices and flight details straight off the page — no paid API, no login
required. Because the page was built for people to look at, not for
programs to read, this part is a bit fragile: if Google changes how the
page looks, this may need fixing.

- **`googleflights.go`** — builds a request (from where, to where, what
  date) and sends it to Google Flights, then hands back both the flight
  options it found and the raw page (kept for the record).
- **`parse.go`** — digs the actual flight details (price, times,
  airline) out of the messy data bundled inside the page. This is the
  "reading tea leaves" part — if it hits something it doesn't recognize,
  it reports an error instead of crashing.
- **`protobuf.go`** — a low-level detail: Google expects the search
  request packed into a specific compact format. This file builds that
  by hand, rather than pulling in a big external library just for this
  one small piece.

## `kafka/`

The messaging glue between the two background workers
(`cmd/agent-worker` and `cmd/collector`). Instead of either one
repeatedly asking "is there anything for me to do yet?", each hands the
next one a tiny note the moment there's something to do — like a sticky
note passed to a coworker instead of them walking over to check your
desk every few seconds. The note never carries the actual work, just an
ID ("go look at request X" / "go run task Y") — the database is still
where the real information lives.

- **`kafka.go`** — the only file. Defines the two message channels
  ("topics"): one for "a request is ready for a decision," one for "a
  search job is ready to run." A worker that decides there's nothing
  more to do for a request just doesn't send a next note — that silence
  is what makes the loop stop. Also handles a subtle but important
  detail: a note is only marked "handled" *after* the work it triggered
  actually finishes, not the moment it's picked up — so if a worker
  crashes partway through, the note comes back and someone else (or the
  same worker, restarted) tries again, instead of the step silently
  never happening.

## `openflights/`

Loads a free, public list of the world's airports (with their
coordinates) and which pairs of them have direct flights between them.
Used to answer "which airports could a connecting flight realistically go
through?" without guessing blindly or asking Google about every airport
on Earth (which would be slow and would look suspicious to Google).

- **`openflights.go`** — downloads that airport/route list once and
  keeps a local copy, then offers simple lookups: "does this pair of
  airports have a direct flight?", "which airports connect both of
  these places?", and "roughly how far apart are two airports?" (used
  only as a rough sanity check, never to guess the actual price).

## `routesearch/`

The actual money-saving logic. Instead of just doing one plain flight
search, it also tries: splitting a trip through a connecting city as two
separate tickets, checking whether a round trip booked together is
cheaper than booking both directions separately, and checking nearby
dates — because any of those can sometimes come out cheaper than the
obvious search. It's careful not to make too many requests to Google, and
it keeps a full written record of every option it considered and why it
was kept or thrown out.

- **`routesearch.go`** — the main search: try the direct flight first,
  then try a short, sensible list of connecting airports (not every
  airport on Earth), checking real prices, and keeping only the winners.
- **`candidates.go`** — helper logic: pick the cheapest valid flight from
  a list, check whether two flights connect properly (long enough
  layover, not absurdly long), and make a smart guess at "is this route
  even worth checking" before spending a real search on it.
- **`roundtrip.go`** — checks whether booking a round trip together is
  cheaper than booking the there-and-back separately, and keeps whichever
  one wins.
- **`flexible.go`** — if the traveler's dates are flexible, first does a
  handful of cheap checks across nearby dates to find the best one, then
  only does the expensive full search on that one best date — instead of
  running the expensive search on every possible date.
- **`timeutil.go`** — small time-math helpers that account for time
  zones properly, so an overnight flight landing the next day doesn't
  accidentally get counted as taking a negative amount of time.
