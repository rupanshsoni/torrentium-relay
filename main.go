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

const privKeyFileName = "private_key"

// === Private key loader (stable PeerID) ===
func loadOrMakePrivateKey() (crypto.PrivKey, error) {
	if b64 := os.Getenv("RELAY_PRIVATE_KEY_B64"); b64 != "" {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("decode key failed: %w", err)
		}
		priv, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("unmarshal key failed: %w", err)
		}
		log.Println("Loaded private key from RELAY_PRIVATE_KEY_B64")
		return priv, nil
	}

	if data, err := os.ReadFile(privKeyFileName); err == nil {
		if priv, err := crypto.UnmarshalPrivateKey(data); err == nil {
			log.Println("Loaded private_key file")
			return priv, nil
		}
	}

	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key failed: %w", err)
	}
	privBytes, _ := crypto.MarshalPrivateKey(priv)
	_ = os.WriteFile(privKeyFileName, privBytes, 0600)
	log.Println("Generated new libp2p private key")
	log.Printf("Base64 (set RELAY_PRIVATE_KEY_B64 to persist):\n%s\n",
		base64.StdEncoding.EncodeToString(privBytes))
	return priv, nil
}

func main() {
	ctx := context.Background()

	// === Render injected port (MUST be used for libp2p) ===
	port := os.Getenv("PORT")
	if port == "" {
		port = "4000"
	}

	// External hostname (Render sets this automatically)
	renderHost := os.Getenv("RENDER_EXTERNAL_HOSTNAME")

	// Build advertised multiaddr
	publicMaddrStr := ""
	if renderHost != "" {
		publicMaddrStr = fmt.Sprintf("/dns4/%s/tcp/443/wss", renderHost)
	}

	priv, err := loadOrMakePrivateKey()
	if err != nil {
		log.Fatalf("key error: %v", err)
	}

	// === libp2p must bind on $PORT ===
	listen := fmt.Sprintf("/ip4/0.0.0.0/tcp/%s/ws", port)

	addrFactory := func(addrs []ma.Multiaddr) []ma.Multiaddr {
		if publicMaddrStr == "" {
			return addrs
		}
		m, err := ma.NewMultiaddr(publicMaddrStr)
		if err != nil {
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
		log.Fatalf("libp2p host failed: %v", err)
	}

	_, err = relay.New(h)
	if err != nil {
		log.Fatalf("enable relay hop failed: %v", err)
	}

	log.Printf("✅ Relay Peer ID: %s", h.ID().String())
	if publicMaddrStr != "" {
		log.Printf("✅ Public relay multiaddr: %s/p2p/%s", publicMaddrStr, h.ID().String())
	}

	// === Internal HTTP status server (not routed by Render) ===
	go func() {
		statusPort := "8080" // any internal port
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte("ok"))
		})
		mux.HandleFunc("/peerid", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(h.ID().String()))
		})
		mux.HandleFunc("/multiaddr", func(w http.ResponseWriter, _ *http.Request) {
			if publicMaddrStr == "" {
				_, _ = w.Write([]byte("no-public-hostname-set"))
				return
			}
			_, _ = w.Write([]byte(fmt.Sprintf("%s/p2p/%s", publicMaddrStr, h.ID().String())))
		})

		log.Printf("Internal status server on :%s", statusPort)
		if err := http.ListenAndServe(":"+statusPort, mux); err != nil {
			log.Printf("status server failed: %v", err)
		}
	}()

	// block forever
	select {
	case <-ctx.Done():
		_ = h.Close()
	case <-time.After(100 * 365 * 24 * time.Hour):
	}
}
