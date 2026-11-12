//go:build ignore

// Gen rewrites the metrics reference in docs/metrics.md from the metric descriptors: go generate ./internal/exporter.
package main

import (
	"log"
	"os"

	"github.com/moond4rk/immich-exporter/internal/exporter"
)

func main() {
	const path = "../../docs/metrics.md"
	doc, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	out, err := exporter.UpdateReference(string(doc))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		log.Fatal(err)
	}
}
