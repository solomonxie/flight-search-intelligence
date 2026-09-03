// Command collector runs the on-demand fare-collection service: consumes
// one route/date request at a time from the Kafka request topic and
// starts a Temporal workflow to fetch it (see DESIGN.md, "Collector
// task queue"). The consumer loop itself stays thin — fetch/retry/
// fan-out logic lives in the Temporal workflow/activity code, not here.
package main

import "fmt"

func main() {
	// TODO: consume a request off the Kafka topic (confluent-kafka-go),
	// start a CollectRouteWorkflow execution keyed by task_id, and
	// commit the offset. No scheduled/broad scraping.
	fmt.Println("flight-search-intelligence: collector (scaffold, not yet implemented)")
}
