// main.go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	relay "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

func main() {
	ctx := context.Background()

	// Render provides the PORT env var
	port := os.Getenv("PORT")
	if port == "" {
		port = "4000"
	}

	listen := fmt.Sprintf("/ip4/0.0.0.0/tcp/%s/ws", port)

	// Create the libp2p host listening on a WebSocket address
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(listen),
	)
	if err != nil {
		log.Fatalf("libp2p new errorty: %v", err)
	}

	// Turn this node into a Relay v2 hop
	_, err = relay.New(h)
	if err != nil {
		log.Fatalf("creating relay service failed: %v", err)
	}

	// Log peer id and suggested public multiaddr (Render terminates TLS at 443,
	// so clients should dial wss at tcp/443 even though we listen on ws)
	log.Printf("Relay Peer ID: %s\n", h.ID().String())

	// Render provides the external hostname in the dashboard. After deploy,
	// build the dialable multiaddr using your render subdomain:
	// /dns4/<your-subdomain>.onrender.com/tcp/443/wss/p2p/<RelayPeerID>
	// We show the local listen addrs too:
	for _, a := range h.Addrs() {
		log.Printf("listening addr: %s\n", a)
	}

	// Small HTTP status server to satisfy Render health checks and let you query the relay id
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/peerid", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(h.ID().String()))
	})

	// Run HTTP server on $PORT in background so the program does not exit.
	go func() {
		log.Printf("HTTP status server starting on :%s", port)
		if err := http.ListenAndServe(":"+port, mux); err != nil {
			log.Printf("HTTP server failed: %v", err)
		}
	}()

	// keep the process alive
	select {
	case <-ctx.Done():
		_ = h.Close()
	case <-time.After(100 * 365 * 24 * time.Hour):
		// effectively forever
	}
}
