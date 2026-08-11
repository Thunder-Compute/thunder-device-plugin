//go:build ignore

// Command thunder-registry-stub stands in for the Thunder Registry API during
// local testing, so the real operator can publish real inventory without a
// Thunder account, an API token or any GPUs.
//
// It serves the endpoints the driver uses, and re-reads its inventory file on
// every request rather than caching it. A test can therefore rewrite
// the file to simulate hardware being enrolled or retired, or capacity policy
// changing, and watch the operator react on its next reconcile.
//
// It deliberately runs outside the cluster: it is scaffolding, not a component,
// and keeping it out of the cluster keeps the deployed manifests honest.
//
// Writes are accepted and recorded rather than applied. A test can read them
// back from /debug/writes, which is how the local test asserts that the
// operator only ever reads: anything that changes Thunder state is the node
// daemon's job, and the operator having write access would widen the blast
// radius of a bug in it.
//
// The resolved listen address is printed to stdout once, so a caller that asks
// for port 0 can discover the port without racing to pick a free one:
//
//	go run hack/thunder-registry-stub.go -inventory inv.json -addr 127.0.0.1:0
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
)

// write is one state-changing request the stub was asked to perform.
type write struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// writeLog records writes so a test can assert on who attempted them.
type writeLog struct {
	mu      sync.Mutex
	entries []write
}

func (l *writeLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, write{Method: r.Method, Path: r.URL.Path})
}

func (l *writeLog) snapshot() []write {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]write(nil), l.entries...)
}

// inventory mirrors the shape of the stub's JSON file. The fields stay raw so
// the stub serves exactly what the file says, without a second copy of the SDK
// types drifting from the real ones.
type inventory struct {
	Zones            json.RawMessage `json:"zones"`
	Hosts            json.RawMessage `json:"hosts"`
	Clients          json.RawMessage `json:"clients"`
	Oversubscription json.RawMessage `json:"oversubscription"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address to listen on; port 0 picks a free port")
	path := flag.String("inventory", "", "path to the inventory JSON file (required)")
	flag.Parse()

	if *path == "" {
		log.Fatal("-inventory is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/zones", serve(*path, func(i inventory) (string, json.RawMessage) {
		return "zones", i.Zones
	}))
	mux.HandleFunc("GET /api/v1/hosts", serve(*path, func(i inventory) (string, json.RawMessage) {
		return "hosts", i.Hosts
	}))
	mux.HandleFunc("GET /api/v1/clients", serve(*path, func(i inventory) (string, json.RawMessage) {
		return "clients", i.Clients
	}))
	mux.HandleFunc("GET /api/v1/zones/{zoneId}/oversubscription-targets", serveOversubscription(*path))

	// Writes are recorded, not applied. Responses are the minimum the SDK needs
	// to keep going, which is enough for a caller to be exercised end to end.
	writes := &writeLog{}
	mux.HandleFunc("POST /api/v1/zones/ensure", func(w http.ResponseWriter, r *http.Request) {
		writes.add(r)
		writeJSON(w, json.RawMessage(`{"zoneId":"zone-local","displayName":"stub"}`))
	})
	mux.HandleFunc("POST /api/v1/enrollment-tokens", func(w http.ResponseWriter, r *http.Request) {
		writes.add(r)
		writeJSON(w, json.RawMessage(`{"enrollmentTokenId":"stub-token-id","enrollmentToken":"stub-token"}`))
	})
	mux.HandleFunc("DELETE /api/v1/enrollment-tokens/{enrollmentTokenId}/node", func(w http.ResponseWriter, r *http.Request) {
		writes.add(r)
		writeJSON(w, json.RawMessage(`{"enrollmentTokenId":"stub-token-id","nodeDeleted":true}`))
	})

	mux.HandleFunc("GET /debug/writes", func(w http.ResponseWriter, r *http.Request) {
		body, err := json.Marshal(writes.snapshot())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, body)
	})

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *addr, err)
	}

	// Announce the real address before serving, so a caller can wait on this
	// line instead of polling for the port to open.
	fmt.Println(listener.Addr().String())
	os.Stdout.Sync()

	log.Fatal(http.Serve(listener, mux))
}

// serve returns a handler that wraps one inventory list in its response object,
// which is how the Thunder API returns collections.
func serve(path string, pick func(inventory) (string, json.RawMessage)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		current, err := read(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		key, value := pick(current)
		if len(value) == 0 {
			value = json.RawMessage("[]")
		}
		writeJSON(w, json.RawMessage(fmt.Sprintf("{%q:%s}", key, value)))
	}
}

func serveOversubscription(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		current, err := read(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		value := current.Oversubscription
		if len(value) == 0 {
			value = json.RawMessage(`{"oversubscriptionTargets":[]}`)
		}
		writeJSON(w, value)
	}
}

func read(path string) (inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return inventory{}, fmt.Errorf("read inventory: %w", err)
	}
	var current inventory
	if err := json.Unmarshal(data, &current); err != nil {
		return inventory{}, fmt.Errorf("parse inventory: %w", err)
	}
	return current, nil
}

func writeJSON(w http.ResponseWriter, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	if _, err := w.Write(body); err != nil {
		log.Printf("write response: %v", err)
	}
}
