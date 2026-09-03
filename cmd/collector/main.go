// Command collector runs the on-demand fare-collection service: consumes
// one route/date request at a time from the Kafka request topic (see
// DESIGN.md) and fetches just that route from a provider.
package main

import "fmt"

func main() {
	// TODO: consume a request off the Kafka topic, fetch that single
	// route/date from a provider, and write the raw result to the S3
	// raw zone (see DESIGN.md). No scheduled/broad scraping.
	fmt.Println("flight-search-intelligence: collector (scaffold, not yet implemented)")
}
