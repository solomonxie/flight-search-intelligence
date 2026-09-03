// Command search-api serves flexible, low-latency flight search (route,
// date range, airline, price, stops...) against the Postgres serving
// store synced from the Delta Lake gold layer (see DESIGN.md). On a
// miss it also publishes a collection request onto the Kafka topic.
package main

import "fmt"

func main() {
	// TODO: serve a search endpoint backed by Postgres; on a miss,
	// publish a request onto the Kafka topic and return "pending".
	fmt.Println("flight-search-intelligence: search-api (scaffold, not yet implemented)")
}
