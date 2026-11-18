package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// MCP tools (pre-serialized tools/list answer)
// ---------------------------------------------------------------------------

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func schema(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type": "object", "properties": props, "required": required,
		"additionalProperties": true,
	}
}

var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		InputSchema: schema(map[string]any{
			"owner":       strProp("Owner login (defaults to the authenticated user)."),
			"name":        strProp("Repository name."),
			"description": strProp("Repository description."),
			"private":     boolProp("Whether the repository is private."),
		}, []string{"name"}),
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Owner login."),
			"repo":  strProp("Repository name."),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Owner login."),
			"repo":  strProp("Repository name."),
			"title": strProp("Issue title."),
			"body":  strProp("Issue body."),
		}, []string{"owner", "repo", "title"}),
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Owner login."),
			"repo":  strProp("Repository name."),
			"state": strProp("Issue state: open, closed or all."),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Owner login."),
			"repo":  strProp("Repository name."),
			"title": strProp("Pull request title."),
			"body":  strProp("Pull request body."),
			"head":  strProp("Head branch name."),
			"base":  strProp("Base branch name."),
		}, []string{"owner", "repo", "title", "head", "base"}),
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request (squash).",
		InputSchema: schema(map[string]any{
			"owner":       strProp("Owner login."),
			"repo":        strProp("Repository name."),
			"pull_number": intProp("Pull request number."),
		}, []string{"owner", "repo", "pull_number"}),
	},
	{
		Name:        "get_issue",
		Description: "Get one issue by number.",
		InputSchema: schema(map[string]any{
			"owner":        strProp("Owner login."),
			"repo":         strProp("Repository name."),
			"issue_number": intProp("Issue number."),
		}, []string{"owner", "repo", "issue_number"}),
	},
	{
		Name:        "list_pull_requests",
		Description: "List pull requests in a repository.",
		InputSchema: schema(map[string]any{
			"owner": strProp("Owner login."),
			"repo":  strProp("Repository name."),
			"state": strProp("Pull request state: open, closed or all."),
		}, []string{"owner", "repo"}),
	},
	{
		Name:        "get_pull_request",
		Description: "Get one pull request by number.",
		InputSchema: schema(map[string]any{
			"owner":       strProp("Owner login."),
			"repo":        strProp("Repository name."),
			"pull_number": intProp("Pull request number."),
		}, []string{"owner", "repo", "pull_number"}),
	},
}

var mcpToolsJSON []byte
var mcpToolsOnce sync.Once

func toolsListJSON() []byte {
	mcpToolsOnce.Do(func() {
		mcpToolsJSON = jsonBytes(map[string]any{"tools": mcpTools})
	})
	return mcpToolsJSON
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing
// ---------------------------------------------------------------------------

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params"`
}

func mcpWriteJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(status)
	w.Write(b)
}

func mcpResult(id json.RawMessage, result any) []byte {
	if len(id) == 0 {
		id = []byte("null")
	}
	return jsonBytes(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result,
	})
}

func mcpError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = []byte("null")
	}
	return jsonBytes(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": code, "message": message},
	})
}

func mcpResultRaw(id json.RawMessage, resultJSON []byte) []byte {
	if len(id) == 0 {
		id = []byte("null")
	}
	var buf strings.Builder
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	buf.Write(id)
	buf.WriteString(`,"result":`)
	buf.Write(resultJSON)
	buf.WriteString(`}`)
	return []byte(buf.String())
}

// ---------------------------------------------------------------------------
// Tool execution (shared with the REST store)
// ---------------------------------------------------------------------------

func mcpStr(args map[string]any, name string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[name].(string); ok {
		return v
	}
	return ""
}

func mcpInt(args map[string]any, name string) (int64, bool) {
	if args == nil {
		return 0, false
	}
	switch v := args[name].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// callMCPTool executes a tool. Returns (result, isError, rpcErrorCode).
// rpcErrorCode != 0 means the call answers a JSON-RPC error instead.
func callMCPTool(name string, args map[string]any) (map[string]any, bool, int) {
	switch name {
	case "create_repo":
		repoName := mcpStr(args, "name")
		if repoName == "" {
			return nil, false, -32602
		}
		owner := mcpStr(args, "owner")
		if owner == "" {
			owner = viewerLogin
		}
		desc := mcpStr(args, "description")
		private := false
		if v, ok := args["private"].(bool); ok {
			private = v
		}
		store.mu.Lock()
		r := store.createRepo(owner, repoName, desc, private)
		if r == nil {
			store.mu.Unlock()
			return map[string]any{"error": "Repository already exists"}, true, 0
		}
		m := repoMap(r)
		store.mu.Unlock()
		return m, false, 0
	case "get_repo":
		owner, repo := mcpStr(args, "owner"), mcpStr(args, "repo")
		if owner == "" || repo == "" {
			return nil, false, -32602
		}
		store.mu.RLock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.RUnlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		m := repoMap(r)
		store.mu.RUnlock()
		return m, false, 0
	case "create_issue":
		owner, repo, title := mcpStr(args, "owner"), mcpStr(args, "repo"), mcpStr(args, "title")
		if owner == "" || repo == "" || title == "" {
			return nil, false, -32602
		}
		body := mcpStr(args, "body")
		store.mu.Lock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.Unlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		is, ok := createIssueLocked(r, title, body, nil)
		if !ok {
			store.mu.Unlock()
			return map[string]any{"error": "cannot create issue"}, true, 0
		}
		m := issueMap(r, is)
		store.mu.Unlock()
		return m, false, 0
	case "list_issues":
		owner, repo := mcpStr(args, "owner"), mcpStr(args, "repo")
		if owner == "" || repo == "" {
			return nil, false, -32602
		}
		state := mcpStr(args, "state")
		if state == "" {
			state = "open"
		}
		store.mu.RLock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.RUnlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		list := sortedIssues(r, state, "desc")
		issues := []any{}
		for _, is := range list {
			issues = append(issues, issueMap(r, is))
		}
		store.mu.RUnlock()
		return map[string]any{"issues": issues}, false, 0
	case "create_pull_request":
		owner, repo, title := mcpStr(args, "owner"), mcpStr(args, "repo"), mcpStr(args, "title")
		head, base := mcpStr(args, "head"), mcpStr(args, "base")
		if owner == "" || repo == "" || title == "" || head == "" || base == "" {
			return nil, false, -32602
		}
		body := mcpStr(args, "body")
		store.mu.Lock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.Unlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		pr, _ := createPullLocked(r, title, body, head, base)
		if pr == nil {
			store.mu.Unlock()
			return map[string]any{"error": "cannot create pull request"}, true, 0
		}
		m := prMap(r, pr)
		store.mu.Unlock()
		return m, false, 0
	case "merge_pull_request":
		owner, repo := mcpStr(args, "owner"), mcpStr(args, "repo")
		num, ok := mcpInt(args, "pull_number")
		if owner == "" || repo == "" || !ok {
			return nil, false, -32602
		}
		store.mu.Lock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.Unlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		pr := r.PRByNum[int(num)]
		if pr == nil {
			store.mu.Unlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		if pr.State != "open" || prHasConflict(r, pr) {
			store.mu.Unlock()
			return map[string]any{"error": "pull request cannot be merged"}, true, 0
		}
		sha, ok2 := mergePullLocked(r, pr)
		store.mu.Unlock()
		if !ok2 {
			return map[string]any{"error": "merge failed"}, true, 0
		}
		return map[string]any{
			"merged": true, "sha": sha, "number": num,
			"message": "Merged pull request #" + strconv.Itoa(int(num)),
		}, false, 0
	case "get_issue":
		owner, repo := mcpStr(args, "owner"), mcpStr(args, "repo")
		num, ok := mcpInt(args, "issue_number")
		if owner == "" || repo == "" || !ok {
			return nil, false, -32602
		}
		store.mu.RLock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.RUnlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		is := r.IssueByNum[int(num)]
		if is == nil {
			store.mu.RUnlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		m := issueMap(r, is)
		store.mu.RUnlock()
		return m, false, 0
	case "list_pull_requests":
		owner, repo := mcpStr(args, "owner"), mcpStr(args, "repo")
		if owner == "" || repo == "" {
			return nil, false, -32602
		}
		state := mcpStr(args, "state")
		if state == "" {
			state = "open"
		}
		store.mu.RLock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.RUnlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		var list []any
		for _, pr := range r.PRs {
			if state != "all" && pr.State != state {
				continue
			}
			list = append(list, prMap(r, pr))
		}
		store.mu.RUnlock()
		if list == nil {
			list = []any{}
		}
		return map[string]any{"pull_requests": list}, false, 0
	case "get_pull_request":
		owner, repo := mcpStr(args, "owner"), mcpStr(args, "repo")
		num, ok := mcpInt(args, "pull_number")
		if owner == "" || repo == "" || !ok {
			return nil, false, -32602
		}
		store.mu.RLock()
		r := store.getRepo(owner, repo)
		if r == nil {
			store.mu.RUnlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		pr := r.PRByNum[int(num)]
		if pr == nil {
			store.mu.RUnlock()
			return map[string]any{"error": "Not Found"}, true, 0
		}
		m := prMap(r, pr)
		store.mu.RUnlock()
		return m, false, 0
	}
	return nil, false, -32602
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func handleMCP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodDelete {
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodPost {
		mcpWriteJSON(w, http.StatusMethodNotAllowed, mcpError(nil, -32600, "method not allowed"))
		return
	}
	// Auth first: any non-empty Bearer token maps to the viewer.
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(auth[7:]) == "" {
		mcpWriteJSON(w, http.StatusUnauthorized, mcpError(nil, -32001, "Unauthorized"))
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		mcpWriteJSON(w, http.StatusBadRequest, mcpError(nil, -32700, "Parse error"))
		return
	}
	var rpc mcpRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&rpc); err != nil {
		mcpWriteJSON(w, http.StatusBadRequest, mcpError(nil, -32700, "Parse error"))
		return
	}
	// Notifications (no id) are answered 202 with no body.
	if len(rpc.ID) == 0 {
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch rpc.Method {
	case "server/discover":
		result := map[string]any{
			"supportedVersions": []string{"2026-07-28"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
			"instructions":      "GitHub-compatible git forge.",
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge", "version": "1.0.0"},
			},
		}
		mcpWriteJSON(w, http.StatusOK, mcpResult(rpc.ID, result))
	case "initialize":
		version := "2026-07-28"
		if params := rpc.Params; params != nil {
			if v, ok := params["protocolVersion"].(string); ok && v != "" {
				version = v
			}
		}
		result := map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		}
		mcpWriteJSON(w, http.StatusOK, mcpResult(rpc.ID, result))
	case "tools/list":
		mcpWriteJSON(w, http.StatusOK, mcpResultRaw(rpc.ID, toolsListJSON()))
	case "tools/call":
		params := rpc.Params
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, isErr, rpcErr := callMCPTool(name, args)
		if rpcErr != 0 {
			mcpWriteJSON(w, http.StatusOK, mcpError(rpc.ID, rpcErr, "invalid params"))
			return
		}
		text := jsonBytes(result)
		out := map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": string(text)},
			},
			"structuredContent": result,
		}
		if isErr {
			out["isError"] = true
		}
		mcpWriteJSON(w, http.StatusOK, mcpResult(rpc.ID, out))
	case "ping":
		mcpWriteJSON(w, http.StatusOK, mcpResult(rpc.ID, map[string]any{}))
	default:
		mcpWriteJSON(w, http.StatusOK, mcpError(rpc.ID, -32601, "method not found: "+rpc.Method))
	}
}
