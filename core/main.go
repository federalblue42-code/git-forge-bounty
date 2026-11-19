// Command forge is the github-core-clone-01 entry: one process serving git smart
// HTTP, REST v3, GraphQL and MCP from its own in-memory store.
//
// Lifecycle (the suite's contract):
//  1. bind an ephemeral port (never 8080),
//  2. POST {"port": N} to $MAKEFUN_ENV_URL/register,
//  3. serve until GET $MAKEFUN_ENV_URL/done answers {"done": true},
//  4. exit 0.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	now := os.Getenv("MAKEFUN_TRIAL_NOW")
	if now == "" {
		now = "2026-11-14T06:30:00Z"
	}

	store := NewStore(now)
	srv := &Server{store: store, token: token}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	go srv.Serve(ln)

	if envURL != "" {
		if err := register(envURL, port); err != nil {
			fmt.Fprintf(os.Stderr, "forge: register: %v\n", err)
			os.Exit(1)
		}
		waitDone(envURL)
	} else {
		// Manual run (no suite beside us): serve until killed.
		select {}
	}
	os.Exit(0)
}

func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		resp, err := http.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 409 {
				return nil
			}
			lastErr = fmt.Errorf("register status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return lastErr
}

func waitDone(envURL string) {
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var payload struct {
				Done bool `json:"done"`
			}
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if json.Unmarshal(data, &payload) == nil && payload.Done {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}
