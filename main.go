package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func handleHealthz(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func rootHandler(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	switch {
	case path == "/healthz":
		handleHealthz(w, req)
	case path == "/mcp":
		handleMCP(w, req)
	case path == "/api/graphql":
		if req.Method == http.MethodPost {
			handleGraphQL(w, req)
			return
		}
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	case strings.HasPrefix(path, "/api/v3"):
		handleREST(w, req)
	case strings.HasSuffix(path, "/info/refs"):
		// /{owner}/{repo}.git/info/refs
		owner, repo, ok := splitGitPath(strings.TrimSuffix(path, "/info/refs"))
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if req.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleInfoRefs(w, req, owner, repo)
	case strings.HasSuffix(path, "/git-upload-pack"):
		owner, repo, ok := splitGitPath(strings.TrimSuffix(path, "/git-upload-pack"))
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if req.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleUploadPack(w, req, owner, repo)
	case strings.HasSuffix(path, "/git-receive-pack"):
		owner, repo, ok := splitGitPath(strings.TrimSuffix(path, "/git-receive-pack"))
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if req.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleReceivePack(w, req, owner, repo)
	default:
		// Unknown route: JSON 404 (never HTML).
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}
}

// splitGitPath parses "/{owner}/{repo}[.git]" into owner and repo.
func splitGitPath(p string) (string, string, bool) {
	p = strings.Trim(p, "/")
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	if parts[0] == "" || repo == "" {
		return "", "", false
	}
	return parts[0], repo, true
}

// ---------------------------------------------------------------------------
// Lifecycle: bind an ephemeral port, register with the env, poll /done.
// Without MAKEFUN_ENV_URL (local runs) just serve on $PORT (default 18080).
// ---------------------------------------------------------------------------

const (
	requestTimeout = 30 * time.Second
	retryInitial   = 250 * time.Millisecond
	retryMax       = 2 * time.Second
)

func sleep(d time.Duration) { time.Sleep(d) }

func retryDelay(attempt int) time.Duration {
	d := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

func register(envURL string, port int) error {
	client := &http.Client{Timeout: requestTimeout}
	for attempt := 0; ; attempt++ {
		body, _ := json.Marshal(map[string]any{"port": port})
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		sleep(retryDelay(attempt))
	}
}

func waitForDone(envURL string) {
	client := &http.Client{Timeout: requestTimeout}
	for attempt := 0; ; attempt++ {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			decErr := json.NewDecoder(res.Body).Decode(&body)
			res.Body.Close()
			if decErr == nil && body.Done {
				return
			}
		}
		sleep(retryDelay(attempt))
	}
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  0,
	}

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		// Local mode: serve on $PORT or 18080, no registration.
		port := os.Getenv("PORT")
		if port == "" {
			port = "18080"
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "[forge] listening on port %s (local mode)\n", port)
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
			os.Exit(1)
		}
		return
	}

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", port)
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
			os.Exit(1)
		}
	}()
	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] register: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[forge] registered; waiting for done\n")
	waitForDone(envURL)
	server.Close()
	fmt.Fprintf(os.Stderr, "[forge] done\n")
}
