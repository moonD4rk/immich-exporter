//go:build ignore

// Gen writes dashboard.json from the dashboard defined in this package: go generate ./grafana.
package main

import (
	"log"
	"os"

	"github.com/moond4rk/immich-exporter/grafana"
)

func main() {
	b, err := grafana.JSON()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("dashboard.json", b, 0o644); err != nil {
		log.Fatal(err)
	}
}
