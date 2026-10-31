// client runs each transport example against the separately started demo server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/kart-io/goagent-examples/orchestration/httpagent/internal/scenarios"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:8097", "example server URL")
	kind := flag.String("case", "all", "all, invoke, sse, ws, ws-cancel, webhook")
	input := flag.String("input", "查询订单 E100", "demo input")
	flag.Parse()
	kinds := []string{*kind}
	if *kind == "all" {
		kinds = []string{"invoke", "sse", "ws", "webhook", "ws-cancel"}
	}
	encoder := json.NewEncoder(os.Stdout)
	for _, name := range kinds {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result, err := scenarios.Run(ctx, http.DefaultClient, *base, name, *input)
		cancel()
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		if err = encoder.Encode(result); err != nil {
			log.Fatal(err)
		}
	}
}
