// Command collector runs the data-collection/scraping service: fetches
// flight fares from providers and writes them to Postgres for
// cmd/search-api to query.
package main

import "fmt"

func main() {
	// TODO: scrape flight fares from provider(s) on a schedule and
	// persist results via internal/db (see internal/db/migrations/0001_init.sql).
	fmt.Println("flight-search-intelligence: collector (scaffold, not yet implemented)")
}
