// MCP over Streamable HTTP at /mcp: JSON-RPC 2.0, the modern 2026-07-28 era
// (server/discover) and the legacy initialize handshake, six tools on the same
// store REST reads.
package main

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
)

const mcpToolsJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new GitHub repository owned by the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"description":{"type":"string"},"private":{"type":"boolean"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository, optionally filtered by state.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string","enum":["open","closed","all"]}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Open a pull request from a head branch pushed over git.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge an open pull request (squash).","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"pull_number":{"type":"integer"}},"required":["owner","repo","pull_number"]}}` +
	`]}`

func (s *Server) mcpRoute(conn net.Conn, req *request, keepAlive bool) {
	if !s.authed(req) {
		s.respond(conn, 401, []byte(`{"message":"Bad credentials"}`), keepAlive)
		return
	}
	if req.method != "POST" {
		s.respond(conn, 405, []byte(`{"message":"Method Not Allowed"}`), keepAlive)
		return
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(req.body, &msg); err != nil {
		s.respond(conn, 400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`), keepAlive)
		return
	}
	if msg.Method == "notifications/initialized" || strings.HasPrefix(msg.Method, "notifications/") {
		s.respond(conn, 202, nil, keepAlive)
		return
	}
	result, rpcErr := s.mcpDispatch(msg.Method, msg.Params)
	var out []byte
	if rpcErr != nil {
		out, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(msg.ID), "error": rpcErr})
	} else {
		out, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(msg.ID), "result": result})
	}
	s.respond(conn, 200, out, keepAlive, "MCP-Protocol-Version: 2026-07-28")
}

func rawOrNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) mcpDispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "server/discover":
		return map[string]any{
			"protocolVersion":   "2026-07-28",
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":        map[string]any{"name": "forge-mcp", "version": "1.0.0"},
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"instructions":      "GitHub-compatible forge tools: repositories, issues and pull requests.",
		}, nil
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		ver := p.ProtocolVersion
		switch ver {
		case "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2026-07-28":
		default:
			ver = "2025-11-25"
		}
		return map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "forge-mcp", "version": "1.0.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var v any
		json.Unmarshal([]byte(mcpToolsJSON), &v)
		return v, nil
	case "tools/call":
		return s.mcpToolCall(params)
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + method}
	}
}

func mcpText(v any) any {
	data, _ := json.Marshal(v)
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(data)}},
		"isError": false,
	}
}

func mcpErrText(msg string) any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

func (s *Server) mcpToolCall(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, &rpcError{Code: -32602, Message: "Invalid params"}
	}
	// suffix match, case-insensitive (a server prefix like github__ is allowed)
	tool := strings.ToLower(p.Name)
	if i := strings.LastIndex(tool, "__"); i >= 0 {
		tool = tool[i+2:]
	}
	args := make(map[string]any)
	json.Unmarshal(p.Arguments, &args)
	str := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	switch tool {
	case "create_repo":
		name := str("name")
		if name == "" {
			return mcpErrText("name is required"), nil
		}
		desc := str("description")
		private, _ := args["private"].(bool)
		r, ok := s.store.createRepo(name, desc, private)
		if !ok {
			return mcpErrText("repository already exists"), nil
		}
		return mcpText(s.store.repoJSON(r)), nil
	case "get_repo":
		owner, repo := str("owner"), str("repo")
		if owner == "" || repo == "" {
			if full := str("full_name"); full != "" {
				if i := strings.IndexByte(full, '/'); i >= 0 {
					owner, repo = full[:i], full[i+1:]
				}
			}
		}
		if owner == "" || repo == "" {
			return mcpErrText("owner and repo are required"), nil
		}
		r := s.store.repo(owner, repo)
		if r == nil {
			return mcpErrText("repository not found"), nil
		}
		return mcpText(s.store.repoJSON(r)), nil
	case "create_issue":
		owner, repo, title := str("owner"), str("repo"), str("title")
		if owner == "" || repo == "" || title == "" {
			return mcpErrText("owner, repo and title are required"), nil
		}
		r := s.store.repo(owner, repo)
		if r == nil {
			return mcpErrText("repository not found"), nil
		}
		it := &Issue{
			Number: r.NextIssue, ID: s.store.id(), NodeID: nodeID("I", s.store.nextID),
			Title: title, Body: str("body"), State: "open", Labels: []*Label{},
			User: viewer, CreatedAt: s.store.now, UpdatedAt: s.store.now,
		}
		r.NextIssue++
		r.Issues = append(r.Issues, it)
		return mcpText(s.store.issueJSON(r, it)), nil
	case "list_issues":
		owner, repo := str("owner"), str("repo")
		if owner == "" || repo == "" {
			return mcpErrText("owner and repo are required"), nil
		}
		r := s.store.repo(owner, repo)
		if r == nil {
			return mcpErrText("repository not found"), nil
		}
		state := str("state")
		if state == "" {
			state = "open"
		}
		items := make([]any, 0)
		for _, it := range r.Issues {
			if state == "all" || it.State == state {
				items = append(items, s.store.issueJSON(r, it))
			}
		}
		return mcpText(items), nil
	case "create_pull_request":
		owner, repo, title, head, base := str("owner"), str("repo"), str("title"), str("head"), str("base")
		if owner == "" || repo == "" || title == "" || head == "" || base == "" {
			return mcpErrText("owner, repo, title, head and base are required"), nil
		}
		r := s.store.repo(owner, repo)
		if r == nil {
			return mcpErrText("repository not found"), nil
		}
		if i := strings.IndexByte(head, ':'); i >= 0 {
			head = head[i+1:]
		}
		headSHA, baseSHA := r.Refs["refs/heads/"+head], r.Refs["refs/heads/"+base]
		if headSHA == "" || baseSHA == "" {
			return mcpErrText("head or base branch does not exist"), nil
		}
		pl := &Pull{
			Number: r.NextPull, ID: s.store.id(), NodeID: nodeID("PR", s.store.nextID),
			Title: title, Body: str("body"), State: "open", Mergeable: true,
			HeadRef: head, BaseRef: base, HeadSHA: headSHA, BaseSHA: baseSHA,
			User: viewer, CreatedAt: s.store.now, UpdatedAt: s.store.now,
		}
		r.NextPull++
		r.Pulls = append(r.Pulls, pl)
		return mcpText(s.store.pullJSON(r, pl)), nil
	case "merge_pull_request":
		owner, repo := str("owner"), str("repo")
		if owner == "" || repo == "" {
			return mcpErrText("owner and repo are required"), nil
		}
		num := 0
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case string:
			num, _ = strconv.Atoi(v)
		}
		r := s.store.repo(owner, repo)
		if r == nil {
			return mcpErrText("repository not found"), nil
		}
		var pl *Pull
		for _, x := range r.Pulls {
			if x.Number == num {
				pl = x
				break
			}
		}
		if pl == nil {
			return mcpErrText("pull request not found"), nil
		}
		if pl.State != "open" || pl.Merged {
			return mcpErrText("pull request is not mergeable"), nil
		}
		baseRef := "refs/heads/" + pl.BaseRef
		curBase := r.Refs[baseRef]
		headSHA := r.Refs["refs/heads/"+pl.HeadRef]
		if headSHA == "" {
			headSHA = pl.HeadSHA
		}
		headMeta := s.store.commitMeta(r, headSHA)
		if headMeta == nil {
			return mcpErrText("head branch has no commits"), nil
		}
		msg := pl.Title + " (#" + strconv.Itoa(pl.Number) + ")"
		sha := s.store.putObject(r, "commit", s.store.commitBytes(headMeta.Tree, []string{curBase}, msg))
		r.Refs[baseRef] = sha
		r.fetchCache = nil
		pl.Merged = true
		pl.State = "closed"
		pl.MergeSHA = sha
		pl.ClosedAt = &s.store.now
		pl.MergedAt = &s.store.now
		return mcpText(map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"}), nil
	default:
		return nil, &rpcError{Code: -32602, Message: "Unknown tool: " + p.Name}
	}
}
