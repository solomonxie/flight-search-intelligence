// Pre-search cost confirmation: the search is unbounded/exhaustive by
// default now (see README "Why this beats a plain flight search"), so a
// request that looks small can still turn into a long run. Before
// spending a single real scrape, print a worst-case route/query/time
// estimate and ask to proceed — the chance to back out, or rerun with
// -budget/-yes, before committing.
package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"
)

// confirmBeforeSearching prints the estimate and reads a y/n answer.
// candidateRoutes is the number of distinct hub routes under
// consideration (display only); maxQueries is the worst-case scrape
// count the caller already computed for whatever mode is about to run.
func confirmBeforeSearching(w io.Writer, r *bufio.Reader, describe string, candidateRoutes, maxQueries int, delay time.Duration) (bool, error) {
	return confirmFreeform(w, r, fmt.Sprintf(
		"Based on your request (%s): %d candidate hub route(s) to search, up to %d scrapes total — roughly %s at the current pacing (often less once pruning kicks in; unbounded by default — see README).",
		describe, candidateRoutes, maxQueries, humanDuration(time.Duration(maxQueries)*delay)))
}

// confirmFreeform prints an arbitrary estimate message plus the y/n
// prompt and reads the answer — the shared tail confirmBeforeSearching
// uses, also used directly where no clean candidate-count estimate
// exists (e.g. N-hop search, see main.go).
func confirmFreeform(w io.Writer, r *bufio.Reader, message string) (bool, error) {
	fmt.Fprint(w, message+" Search now? [y/N] ")
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

// humanDuration renders a rough estimate at whatever granularity is
// actually legible — seconds/minutes for a short run, hours for a long
// one, fractional days once it crosses 24h.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return d.Round(time.Second).String()
	case d < 24*time.Hour:
		return d.Round(time.Minute).String()
	default:
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	}
}

// maxQueriesForDirection is the worst-case scrape count for one
// direction's hub search: one baseline query, plus up to two (leg1,
// leg2) for every surviving candidate hub — the upper bound Search
// (search.go) can spend before its own (*) frontier-cutoff or a
// positive QueryBudget cuts it short.
func maxQueriesForDirection(candidateHubs int) int {
	return 1 + 2*candidateHubs
}
