// main.go
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
	"crypto/rand"

	libp2p "github.com/libp2p/go-libp2p"
	relay "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
	crypto "github.com/libp2p/go-libp2p/core/crypto"
)

const (
	// file fallback if you want local persistence (optional)
	privKeyFileName = "private_key"
)

func loadOrMakePrivateKey() (crypto.PrivKey, error) {
	// 1) If user provided RELAY_PRIVATE_KEY_B64 env var, prefer that (stable peer ID across redeploys)
	if b64 := os.Getenv("RELAY_PRIVATE_KEY_B64"); b64 != "" {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("failed to decode RELAY_PRIVATE_KEY_B64: %w", err)
		}
		priv, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal private key from RELAY_PRIVATE_KEY_B64: %w", err)
		}
		log.Println("Loaded private key from RELAY_PRIVATE_KEY_B64")
		return priv, nil
	}

	// 2) Try reading private_key file (useful for local dev or manually persisting)
	if data, err := os.ReadFile(privKeyFileName); err == nil {
		if priv, err := crypto.UnmarshalPrivateKey(data); err == nil {
			log.Println("Loaded existing private_key file")
			return priv, nil
		}
		// if unmarshal fails, we'll generate a new one below
	}

	// 3) Otherwise generate a new key (Ed25519)
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key failed: %w", err)
	}
	// marshal bytes (so user can persist if they want)
	privBytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal private key failed: %w", err)
	}
	// attempt to write file (best-effort)
	_ = os.WriteFile(privKeyFileName, privBytes, 0600)
	// print b64 to logs so user can copy into Render env var for stability if desired
	log.Println("Generated new libp2p private key.")
	log.Printf("Base64 (set RELAY_PRIVATE_KEY_B64 to this to persist PeerID across redeploys):\n%s\n",
		base64.StdEncoding.EncodeToString(privBytes))
	return priv, nil
}

func main() {
	ctx := context.Background()

	// Health server PORT (Render injects this)
	httpPort := os.Getenv("PORT")
	if httpPort == "" {
		httpPort = "4000"
	}

	// Internal WS port for libp2p (must NOT be $PORT on Render). Use an env var if you want to change this.
	libp2pPort := os.Getenv("LIBP2P_WS_PORT")
	if libp2pPort == "" {
		libp2pPort = "10001"
	}

	// Get the Render external hostname. On Render this is set automatically to e.g. torrentium-relay-1.onrender.com
	renderHost := os.Getenv("RENDER_EXTERNAL_HOSTNAME")
	if renderHost == "" {
		// Allow override for local testing
		renderHost = os.Getenv("PUBLIC_HOST")
	}
	if renderHost == "" {
		log.Println("Warning: RENDER_EXTERNAL_HOSTNAME not set; if running on Render the relay will not advertise a proper dns4 address.")
	}

	// Build the public (advertised) multiaddr the clients should dial:
	publicMaddrStr := fmt.Sprintf("/dns4/%s/tcp/443/wss", renderHost)

	// Load or generate private key
	priv, err := loadOrMakePrivateKey()
	if err != nil {
		log.Fatalf("private key error: %v", err)
	}

	// internal listen addr: we accept plain ws inside container; Render terminates TLS for us on 443
	listen := fmt.Sprintf("/ip4/0.0.0.0/tcp/%s/ws", libp2pPort)

	// Create host with AddrsFactory that advertises only the public dns4/wss address (so Torrentium clients see exactly /dns4/.../tcp/443/wss)
	addrFactory := func(addrs []ma.Multiaddr) []ma.Multiaddr {
		if renderHost == "" {
			// fallback: return whatever addresses the host had
			return addrs
		}
		m, err := ma.NewMultiaddr(publicMaddrStr)
		if err != nil {
			log.Printf("failed to build public multiaddr: %v", err)
			return addrs
		}
		return []ma.Multiaddr{m}
	}

	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(listen),
		libp2p.AddrsFactory(addrFactory),
		libp2p.ForceReachabilityPublic(),
	)
	if err != nil {
		log.Fatalf("failed to create libp2p host: %v", err)
	}

	// Make this node a Circuit Relay v2 hop (accept reservations)
	_, err = relay.New(h)
	if err != nil {
		log.Fatalf("failed to enable relay hop: %v", err)
	}

	// Log peer ID and public multiaddr (what Torrentium clients should use)
	log.Printf("✅ Relay Peer ID: %s", h.ID().String())
	if renderHost != "" {
		log.Printf("✅ Public relay multiaddr: %s/p2p/%s", publicMaddrStr, h.ID().String())
	} else {
		// show the host addrs we actually bound to
		for _, a := range h.Addrs() {
			log.Printf("libp2p listening addr: %s", a.String())
		}
	}

	// Simple HTTP health/status server on $PORT (Render health checks + quick verification)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/peerid", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(h.ID().String()))
	})
	mux.HandleFunc("/multiaddr", func(w http.ResponseWriter, _ *http.Request) {
		if renderHost == "" {
			_, _ = w.Write([]byte("no-public-hostname-set"))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf("%s/p2p/%s", publicMaddrStr, h.ID().String())))
	})

	go func() {
		log.Printf("HTTP status server starting on :%s", httpPort)
		if err := http.ListenAndServe(":"+httpPort, mux); err != nil {
			log.Printf("HTTP server failed: %v", err)
		}
	}()

	// Block forever
	select {
	case <-ctx.Done():
		_ = h.Close()
	case <-time.After(100 * 365 * 24 * time.Hour):
	}
}
