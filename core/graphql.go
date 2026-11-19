// GraphQL v4 at /api/graphql: a small parser for the documents gh and
// github-mcp-server send, resolved on the same store, with the introspection
// the suite asks for (__type on Issue, PullRequest, Repository, SearchType).
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- parser

type gqlValue struct {
	kind string // "var","string","int","float","bool","enum","list","object","null"
	str  string
	num  float64
	b    bool
	list []gqlValue
	obj  map[string]gqlValue
}

type gqlField struct {
	alias    string
	name     string
	args     map[string]gqlValue
	children []*gqlField
}

type gqlOp struct {
	kind    string // query | mutation
	name    string
	varDefs map[string]string
	fields  []*gqlField
}

type gqlParser struct {
	toks []string
	pos  int
	vars map[string]any
}

func gqlTokenize(s string) []string {
	var toks []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
					switch s[j] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(s[j])
					}
				} else {
					b.WriteByte(s[j])
				}
				j++
			}
			toks = append(toks, "\""+b.String()+"\"")
			i = j + 1
		case strings.ContainsRune("{}()[]:!$=@|", rune(c)):
			toks = append(toks, string(c))
			i++
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\n\r,{}()[]:!$=@|\"", rune(s[j])) {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks
}

func (p *gqlParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *gqlParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *gqlParser) expect(t string) bool {
	if p.peek() == t {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseValue() gqlValue {
	t := p.peek()
	switch {
	case t == "$":
		p.next()
		return gqlValue{kind: "var", str: p.next()}
	case strings.HasPrefix(t, "\""):
		p.next()
		return gqlValue{kind: "string", str: t[1 : len(t)-1]}
	case t == "[":
		p.next()
		var list []gqlValue
		for p.peek() != "]" && p.peek() != "" {
			list = append(list, p.parseValue())
		}
		p.expect("]")
		return gqlValue{kind: "list", list: list}
	case t == "{":
		p.next()
		obj := make(map[string]gqlValue)
		for p.peek() != "}" && p.peek() != "" {
			k := p.next()
			p.expect(":")
			obj[k] = p.parseValue()
		}
		p.expect("}")
		return gqlValue{kind: "object", obj: obj}
	case t == "true" || t == "false":
		p.next()
		return gqlValue{kind: "bool", b: t == "true"}
	case t == "null":
		p.next()
		return gqlValue{kind: "null"}
	default:
		p.next()
		if n, err := strconv.ParseFloat(t, 64); err == nil {
			return gqlValue{kind: "int", num: n}
		}
		return gqlValue{kind: "enum", str: t}
	}
}

func (p *gqlParser) resolve(v gqlValue) any {
	switch v.kind {
	case "var":
		return p.vars[v.str]
	case "string":
		return v.str
	case "int", "float":
		return v.num
	case "bool":
		return v.b
	case "enum":
		return v.str
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, x := range v.list {
			out = append(out, p.resolve(x))
		}
		return out
	case "object":
		out := make(map[string]any)
		for k, x := range v.obj {
			out[k] = p.resolve(x)
		}
		return out
	}
	return nil
}

func (p *gqlParser) parseArgs() map[string]gqlValue {
	args := make(map[string]gqlValue)
	if !p.expect("(") {
		return args
	}
	for p.peek() != ")" && p.peek() != "" {
		k := p.next()
		p.expect(":")
		args[k] = p.parseValue()
	}
	p.expect(")")
	return args
}

func (p *gqlParser) parseSelectionSet() []*gqlField {
	var out []*gqlField
	if !p.expect("{") {
		return out
	}
	for p.peek() != "}" && p.peek() != "" {
		if p.peek() == "..." {
			// fragment spread or inline fragment: skip the name, take the set
			p.next()
			if p.peek() == "on" {
				p.next()
				p.next()
			} else if p.peek() != "{" && p.peek() != "@" {
				p.next()
			}
			out = append(out, p.parseSelectionSet()...)
			continue
		}
		f := &gqlField{}
		name := p.next()
		if p.expect(":") {
			f.alias = name
			f.name = p.next()
		} else {
			f.name = name
		}
		f.args = p.parseArgs()
		if p.peek() == "{" {
			f.children = p.parseSelectionSet()
		}
		out = append(out, f)
	}
	p.expect("}")
	return out
}

func gqlParse(query string, vars map[string]any) ([]*gqlOp, error) {
	p := &gqlParser{toks: gqlTokenize(query), vars: vars}
	var ops []*gqlOp
	for p.peek() != "" {
		op := &gqlOp{kind: "query", varDefs: make(map[string]string)}
		t := p.peek()
		if t == "query" || t == "mutation" {
			op.kind = p.next()
			if p.peek() != "(" && p.peek() != "{" && p.peek() != "" {
				op.name = p.next()
			}
			if p.expect("(") {
				for p.peek() != ")" && p.peek() != "" {
					p.expect("$")
					vn := p.next()
					p.expect(":")
					tn := p.next()
					op.varDefs[vn] = tn
					if p.expect("=") {
						p.parseValue()
					}
				}
				p.expect(")")
			}
		}
		op.fields = p.parseSelectionSet()
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return ops, nil
}

// ---------------------------------------------------------------- resolver

type gqlCtx struct {
	s     *Server
	store *Store
	errs  []map[string]any
}

func (c *gqlCtx) errf(format string, a ...any) {
	c.errs = append(c.errs, map[string]any{"message": fmt.Sprintf(format, a...)})
}

func argStr(f *gqlField, p *gqlParser, k string) string {
	if v, ok := f.args[k]; ok {
		if s, ok := p.resolve(v).(string); ok {
			return s
		}
	}
	return ""
}

func argInt(f *gqlField, p *gqlParser, k string, def int) int {
	if v, ok := f.args[k]; ok {
		if n, ok := p.resolve(v).(float64); ok {
			return int(n)
		}
	}
	return def
}

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "closedAt", "author", "labels", "comments", "url"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body", "createdAt", "updatedAt", "mergedAt", "author", "url"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url", "isPrivate"},
	"SearchType":  {},
}

func (c *gqlCtx) resolveFields(fields []*gqlField, p *gqlParser, obj map[string]any, typeName string) map[string]any {
	out := make(map[string]any)
	for _, f := range fields {
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			out[key] = typeName
			continue
		}
		if f.name == "__type" {
			tn := argStr(f, p, "name")
			out[key] = c.introspectType(tn)
			continue
		}
		if f.name == "__schema" {
			out[key] = map[string]any{"queryType": map[string]any{"name": "Query"}, "mutationType": map[string]any{"name": "Mutation"}}
			continue
		}
		v, ok := obj[f.name]
		if !ok {
			if known, listed := gqlTypeFields[typeName]; listed {
				found := false
				for _, k := range known {
					if k == f.name {
						found = true
						break
					}
				}
				if !found && typeName != "" {
					c.errf("Cannot query field %q on type %q.", f.name, typeName)
					continue
				}
			}
			out[key] = nil
			continue
		}
		if len(f.children) > 0 {
			switch vv := v.(type) {
			case map[string]any:
				out[key] = c.resolveFields(f.children, p, vv, childType(typeName, f.name))
			case []any:
				items := make([]any, 0, len(vv))
				for _, x := range vv {
					if m, ok := x.(map[string]any); ok {
						items = append(items, c.resolveFields(f.children, p, m, childType(typeName, f.name)))
					} else {
						items = append(items, x)
					}
				}
				out[key] = items
			case *lazyList:
				out[key] = c.resolveList(f, p, vv, typeName)
			case *lazyObj:
				out[key] = c.resolveFields(f.children, p, vv.fn(), childType(typeName, f.name))
			default:
				out[key] = v
			}
		} else {
			switch vv := v.(type) {
			case *lazyObj:
				out[key] = vv.fn()
			case *lazyList:
				out[key] = c.resolveList(f, p, vv, typeName)
			default:
				out[key] = v
			}
		}
	}
	return out
}

type lazyObj struct{ fn func() map[string]any }
type lazyList struct {
	items func() []map[string]any
	total func() int
	typ   string
}

func (c *gqlCtx) resolveList(f *gqlField, p *gqlParser, ll *lazyList, parentType string) map[string]any {
	items := ll.items()
	first := argInt(f, p, "first", len(items))
	if first > len(items) {
		first = len(items)
	}
	nodes := make([]any, 0, first)
	for _, it := range items[:first] {
		if len(f.children) > 0 {
			// find the nodes/edges subselection
			var nodeFields []*gqlField
			for _, ch := range f.children {
				if ch.name == "nodes" {
					nodeFields = ch.children
				}
			}
			if nodeFields != nil {
				nodes = append(nodes, c.resolveFields(nodeFields, p, it, ll.typ))
			} else {
				nodes = append(nodes, it)
			}
		} else {
			nodes = append(nodes, it)
		}
	}
	out := make(map[string]any)
	for _, ch := range f.children {
		switch ch.name {
		case "nodes":
			out["nodes"] = nodes
		case "totalCount":
			out["totalCount"] = ll.total()
		case "pageInfo":
			out["pageInfo"] = map[string]any{"hasNextPage": false, "hasPreviousPage": false, "endCursor": nil, "startCursor": nil}
		default:
			out[ch.name] = nodes
		}
	}
	if len(f.children) == 0 {
		out["nodes"] = nodes
	}
	return out
}

func childType(parent, field string) string {
	switch field {
	case "owner", "author":
		return "User"
	case "issue", "issues":
		return "Issue"
	case "pullRequest", "pullRequests":
		return "PullRequest"
	case "repository":
		return "Repository"
	case "defaultBranchRef":
		return "Ref"
	case "labels":
		return "Label"
	case "files":
		return "PullRequestChangedFile"
	case "nodes":
		return parent
	}
	return ""
}

func (c *gqlCtx) introspectType(name string) any {
	if name == "SearchType" {
		return map[string]any{
			"name": "SearchType",
			"enumValues": []any{
				map[string]any{"name": "ISSUE"}, map[string]any{"name": "REPOSITORY"},
				map[string]any{"name": "USER"}, map[string]any{"name": "DISCUSSION"},
			},
		}
	}
	fields, ok := gqlTypeFields[name]
	if !ok {
		return nil
	}
	out := make([]any, 0, len(fields))
	for _, f := range fields {
		out = append(out, map[string]any{"name": f})
	}
	return map[string]any{"name": name, "fields": out, "enumValues": nil}
}

// ---------------------------------------------------------------- entry

func (s *Server) graphqlRoute(conn net.Conn, req *request, keepAlive bool) {
	if req.method != "POST" {
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	// batch: two operation documents in one body answer two results in order
	trimmed := strings.TrimSpace(string(req.body))
	if strings.HasPrefix(trimmed, "[") {
		var batch []json.RawMessage
		if json.Unmarshal(req.body, &batch) != nil {
			s.respond(conn, 400, []byte(`{"message":"Malformed JSON"}`), keepAlive)
			return
		}
		results := make([]any, 0, len(batch))
		for _, one := range batch {
			results = append(results, s.gqlOne(one))
		}
		out, _ := json.Marshal(results)
		s.respond(conn, 200, out, keepAlive)
		return
	}
	out, _ := json.Marshal(s.gqlOne(req.body))
	s.respond(conn, 200, out, keepAlive)
}

func (s *Server) gqlOne(body []byte) any {
	var doc struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Query == "" {
		return map[string]any{"errors": []any{map[string]any{"message": "Malformed query"}}}
	}
	ops, err := gqlParse(doc.Query, doc.Variables)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	op := ops[0]
	if doc.OperationName != "" {
		for _, o := range ops {
			if o.name == doc.OperationName {
				op = o
				break
			}
		}
	}
	p := &gqlParser{vars: doc.Variables}
	c := &gqlCtx{s: s, store: s.store}
	data := c.resolveRoot(op, p)
	res := map[string]any{"data": data}
	if len(c.errs) > 0 {
		res["errors"] = c.errs
	}
	return res
}

func (c *gqlCtx) resolveRoot(op *gqlOp, p *gqlParser) map[string]any {
	root := make(map[string]any)
	if op.kind == "mutation" {
		for _, f := range op.fields {
			switch f.name {
			case "createPullRequest":
				root[keyOf(f)] = c.mutCreatePullRequest(f, p)
			case "mergePullRequest":
				root[keyOf(f)] = c.mutMergePullRequest(f, p)
			case "createIssue":
				root[keyOf(f)] = c.mutCreateIssue(f, p)
			default:
				c.errf("Cannot query field %q on type %q.", f.name, "Mutation")
			}
		}
		return root
	}
	for _, f := range op.fields {
		switch f.name {
		case "viewer":
			root[keyOf(f)] = c.resolveFields(f.children, p, map[string]any{
				"login": viewerLogin, "id": viewer.NodeID, "databaseId": viewerID, "name": viewer.Name,
			}, "User")
		case "repository":
			owner := argStr(f, p, "owner")
			name := argStr(f, p, "name")
			r := c.store.repo(owner, name)
			if r == nil {
				root[keyOf(f)] = nil
				continue
			}
			root[keyOf(f)] = c.resolveFields(f.children, p, c.repoObj(r, f, p), "Repository")
		case "__type":
			root[keyOf(f)] = c.introspectType(argStr(f, p, "name"))
		case "__schema":
			root[keyOf(f)] = map[string]any{"queryType": map[string]any{"name": "Query"}, "mutationType": map[string]any{"name": "Mutation"}}
		case "__typename":
			root[keyOf(f)] = "Query"
		default:
			c.errf("Cannot query field %q on type %q.", f.name, "Query")
		}
	}
	return root
}

func keyOf(f *gqlField) string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

func (c *gqlCtx) repoObj(r *Repo, f *gqlField, p *gqlParser) map[string]any {
	obj := map[string]any{
		"id": r.NodeID, "databaseId": r.ID, "name": r.Name, "nameWithOwner": r.FullName,
		"description": r.Description, "isPrivate": r.Private,
		"url":              "http://forge.local/" + r.FullName,
		"viewerPermission": "ADMIN", "hasIssuesEnabled": true,
		"owner": map[string]any{"login": viewerLogin, "id": viewer.NodeID},
		"defaultBranchRef": map[string]any{
			"name":   r.DefaultBranch,
			"target": map[string]any{"oid": r.Refs["refs/heads/"+r.DefaultBranch]},
		},
	}
	for _, ch := range f.children {
		switch ch.name {
		case "issue":
			num := argInt(ch, p, "number", 0)
			obj["issue"] = &lazyObj{fn: func() map[string]any {
				for _, it := range r.Issues {
					if it.Number == num {
						return c.issueObj(r, it)
					}
				}
				return nil
			}}
		case "issues":
			obj["issues"] = c.issuesList(r, ch, p)
		case "pullRequest":
			num := argInt(ch, p, "number", 0)
			obj["pullRequest"] = &lazyObj{fn: func() map[string]any {
				for _, pl := range r.Pulls {
					if pl.Number == num {
						return c.pullObj(r, pl)
					}
				}
				return nil
			}}
		case "pullRequests":
			obj["pullRequests"] = c.pullsList(r, ch, p)
		}
	}
	return obj
}

func (c *gqlCtx) issuesList(r *Repo, f *gqlField, p *gqlParser) *lazyList {
	states := map[string]bool{}
	if v, ok := f.args["states"]; ok {
		if list, ok := p.resolve(v).([]any); ok {
			for _, x := range list {
				if s, ok := x.(string); ok {
					states[strings.ToLower(s)] = true
				}
			}
		}
	}
	desc := true
	if v, ok := f.args["orderBy"]; ok {
		if m, ok := p.resolve(v).(map[string]any); ok {
			if dir, ok := m["direction"].(string); ok && dir == "ASC" {
				desc = false
			}
		}
	}
	return &lazyList{
		typ: "Issue",
		items: func() []map[string]any {
			var sel []*Issue
			for _, it := range r.Issues {
				if len(states) == 0 || states[it.State] {
					sel = append(sel, it)
				}
			}
			sort.Slice(sel, func(i, j int) bool {
				if desc {
					return sel[i].Number > sel[j].Number
				}
				return sel[i].Number < sel[j].Number
			})
			out := make([]map[string]any, 0, len(sel))
			for _, it := range sel {
				out = append(out, c.issueObj(r, it))
			}
			return out
		},
		total: func() int {
			n := 0
			for _, it := range r.Issues {
				if len(states) == 0 || states[it.State] {
					n++
				}
			}
			return n
		},
	}
}

func (c *gqlCtx) pullsList(r *Repo, f *gqlField, p *gqlParser) *lazyList {
	states := map[string]bool{}
	if v, ok := f.args["states"]; ok {
		if list, ok := p.resolve(v).([]any); ok {
			for _, x := range list {
				if s, ok := x.(string); ok {
					states[strings.ToUpper(s)] = true
				}
			}
		}
	}
	return &lazyList{
		typ: "PullRequest",
		items: func() []map[string]any {
			out := make([]map[string]any, 0)
			for _, pl := range r.Pulls {
				st := gqlPullState(pl)
				if len(states) == 0 || states[st] {
					out = append(out, c.pullObj(r, pl))
				}
			}
			return out
		},
		total: func() int { return len(r.Pulls) },
	}
}

func gqlPullState(pl *Pull) string {
	if pl.Merged {
		return "MERGED"
	}
	if pl.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func (c *gqlCtx) issueObj(r *Repo, it *Issue) map[string]any {
	state := "OPEN"
	if it.State == "closed" {
		state = "CLOSED"
	}
	labels := make([]map[string]any, 0, len(it.Labels))
	for _, l := range it.Labels {
		labels = append(labels, map[string]any{"name": l.Name, "id": l.NodeID, "color": l.Color})
	}
	return map[string]any{
		"id": it.NodeID, "number": it.Number, "title": it.Title, "body": it.Body,
		"state": state, "createdAt": it.CreatedAt, "updatedAt": it.UpdatedAt, "closedAt": it.ClosedAt,
		"url":      "http://forge.local/" + r.FullName + "/issues/" + strconv.Itoa(it.Number),
		"author":   map[string]any{"login": viewerLogin},
		"labels":   &lazyList{typ: "Label", items: func() []map[string]any { return labels }, total: func() int { return len(labels) }},
		"comments": map[string]any{"totalCount": it.Comments},
	}
}

func (c *gqlCtx) pullObj(r *Repo, pl *Pull) map[string]any {
	mergeable := "MERGEABLE"
	if pl.State == "open" {
		cur := r.Refs["refs/heads/"+pl.BaseRef]
		if cur != pl.BaseSHA {
			bc := c.store.changedPaths(r, pl.BaseSHA, cur)
			hc := c.store.changedPaths(r, pl.BaseSHA, pl.HeadSHA)
			for p := range hc {
				if bc[p] {
					mergeable = "CONFLICTING"
					break
				}
			}
		}
	}
	files := c.store.pullFiles(r, pl)
	fileNodes := make([]map[string]any, 0, len(files))
	for _, fa := range files {
		if m, ok := fa.(map[string]any); ok {
			fileNodes = append(fileNodes, map[string]any{
				"path": m["filename"], "additions": m["additions"], "deletions": m["deletions"],
			})
		}
	}
	return map[string]any{
		"id": pl.NodeID, "number": pl.Number, "title": pl.Title, "body": pl.Body,
		"headRefName": pl.HeadRef, "baseRefName": pl.BaseRef,
		"mergeable": mergeable, "state": gqlPullState(pl), "merged": pl.Merged,
		"createdAt": pl.CreatedAt, "updatedAt": pl.UpdatedAt, "mergedAt": pl.MergedAt,
		"url":    "http://forge.local/" + r.FullName + "/pull/" + strconv.Itoa(pl.Number),
		"author": map[string]any{"login": viewerLogin},
		"files":  &lazyList{typ: "PullRequestChangedFile", items: func() []map[string]any { return fileNodes }, total: func() int { return len(fileNodes) }},
	}
}

func (c *gqlCtx) mutCreatePullRequest(f *gqlField, p *gqlParser) map[string]any {
	input := map[string]any{}
	if v, ok := f.args["input"]; ok {
		if m, ok := p.resolve(v).(map[string]any); ok {
			input = m
		}
	}
	str := func(k string) string { s, _ := input[k].(string); return s }
	repoID := str("repositoryId")
	var r *Repo
	for _, x := range c.store.repoList {
		if x.NodeID == repoID {
			r = x
			break
		}
	}
	if r == nil {
		c.errf("Could not resolve to a Repository with the id %q.", repoID)
		return map[string]any{"pullRequest": nil}
	}
	head, base := str("headRefName"), str("baseRefName")
	headSHA, baseSHA := r.Refs["refs/heads/"+head], r.Refs["refs/heads/"+base]
	if headSHA == "" || baseSHA == "" {
		c.errf("Head or base branch does not exist.")
		return map[string]any{"pullRequest": nil}
	}
	pl := &Pull{
		Number: r.NextPull, ID: c.store.id(), NodeID: nodeID("PR", c.store.nextID),
		Title: str("title"), Body: str("body"), State: "open", Mergeable: true,
		HeadRef: head, BaseRef: base, HeadSHA: headSHA, BaseSHA: baseSHA,
		User: viewer, CreatedAt: c.store.now, UpdatedAt: c.store.now,
	}
	r.NextPull++
	r.Pulls = append(r.Pulls, pl)
	obj := c.pullObj(r, pl)
	for _, ch := range f.children {
		if ch.name == "pullRequest" {
			return map[string]any{"pullRequest": c.resolveFields(ch.children, p, obj, "PullRequest")}
		}
	}
	return map[string]any{"pullRequest": obj}
}

func (c *gqlCtx) mutMergePullRequest(f *gqlField, p *gqlParser) map[string]any {
	input := map[string]any{}
	if v, ok := f.args["input"]; ok {
		if m, ok := p.resolve(v).(map[string]any); ok {
			input = m
		}
	}
	prID, _ := input["pullRequestId"].(string)
	var r *Repo
	var pl *Pull
	for _, x := range c.store.repoList {
		for _, y := range x.Pulls {
			if y.NodeID == prID {
				r, pl = x, y
				break
			}
		}
	}
	if pl == nil || r == nil {
		c.errf("Could not resolve to a PullRequest with the id %q.", prID)
		return map[string]any{"pullRequest": nil}
	}
	baseRef := "refs/heads/" + pl.BaseRef
	curBase := r.Refs[baseRef]
	headSHA := r.Refs["refs/heads/"+pl.HeadRef]
	headMeta := c.store.commitMeta(r, headSHA)
	if headMeta != nil && pl.State == "open" && !pl.Merged {
		msg := pl.Title + " (#" + strconv.Itoa(pl.Number) + ")"
		sha := c.store.putObject(r, "commit", c.store.commitBytes(headMeta.Tree, []string{curBase}, msg))
		r.Refs[baseRef] = sha
		r.fetchCache = nil
		pl.Merged = true
		pl.State = "closed"
		pl.MergeSHA = sha
		pl.ClosedAt = &c.store.now
		pl.MergedAt = &c.store.now
	}
	obj := c.pullObj(r, pl)
	for _, ch := range f.children {
		if ch.name == "pullRequest" {
			return map[string]any{"pullRequest": c.resolveFields(ch.children, p, obj, "PullRequest")}
		}
	}
	return map[string]any{"pullRequest": obj}
}

func (c *gqlCtx) mutCreateIssue(f *gqlField, p *gqlParser) map[string]any {
	input := map[string]any{}
	if v, ok := f.args["input"]; ok {
		if m, ok := p.resolve(v).(map[string]any); ok {
			input = m
		}
	}
	str := func(k string) string { s, _ := input[k].(string); return s }
	repoID := str("repositoryId")
	var r *Repo
	for _, x := range c.store.repoList {
		if x.NodeID == repoID {
			r = x
			break
		}
	}
	if r == nil {
		c.errf("Could not resolve to a Repository with the id %q.", repoID)
		return map[string]any{"issue": nil}
	}
	it := &Issue{
		Number: r.NextIssue, ID: c.store.id(), NodeID: nodeID("I", c.store.nextID),
		Title: str("title"), Body: str("body"), State: "open", Labels: []*Label{},
		User: viewer, CreatedAt: c.store.now, UpdatedAt: c.store.now,
	}
	r.NextIssue++
	r.Issues = append(r.Issues, it)
	obj := c.issueObj(r, it)
	for _, ch := range f.children {
		if ch.name == "issue" {
			return map[string]any{"issue": c.resolveFields(ch.children, p, obj, "Issue")}
		}
	}
	return map[string]any{"issue": obj}
}
