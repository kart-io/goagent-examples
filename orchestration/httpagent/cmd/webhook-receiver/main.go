// webhook-receiver prints adapter notifications; it is a local demo sink.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/kart-io/goagent-contrib/httpagent"
)

func newReceiver(output io.Writer) http.Handler {
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				http.Error(w, "event too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "cannot read event", http.StatusBadRequest)
			}
			return
		}
		var event httpagent.RunEvent
		if err = json.Unmarshal(data, &event); err != nil || event.SchemaVersion != 1 || event.EventID == "" || event.RunID == "" || event.EventType == "" {
			http.Error(w, "invalid event envelope", http.StatusBadRequest)
			return
		}
		mu.Lock()
		err = json.NewEncoder(output).Encode(event)
		mu.Unlock()
		if err != nil {
			http.Error(w, "cannot record event", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "local callback address")
	flag.Parse()
	server := &http.Server{Addr: *listen, Handler: newReceiver(os.Stdout), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("Webhook receiver: http://%s/events (stdout only; no persistence or deduplication)", *listen)
	log.Fatal(server.ListenAndServe())
}
