// Pull requests (create, read, merge with squash commits) and the contents API
// (PUT/GET/DELETE building real git objects on the default branch).
package main

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"path"
	"strconv"
	"strings"
)

func (s *Server) pullsRoute(conn net.Conn, req *request, keepAlive bool, r *Repo, rest []string) {
	m := req.method
	if len(rest) == 0 {
		switch m {
		case "GET":
			items := make([]any, 0)
			for _, pl := range r.Pulls {
				items = append(items, s.store.pullJSON(r, pl))
			}
			out, _ := json.Marshal(items)
			s.respond(conn, 200, out, keepAlive)
		case "POST":
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Head  string `json:"head"`
				Base  string `json:"base"`
				Draft bool   `json:"draft"`
			}
			if err := json.Unmarshal(req.body, &body); err != nil || body.Title == "" || body.Head == "" || body.Base == "" {
				s.respond(conn, 422, []byte(`{"message":"Validation Failed"}`), keepAlive)
				return
			}
			head := body.Head
			if i := strings.IndexByte(head, ':'); i >= 0 {
				head = head[i+1:]
			}
			headSHA := r.Refs["refs/heads/"+head]
			baseSHA := r.Refs["refs/heads/"+body.Base]
			if headSHA == "" || baseSHA == "" {
				s.respond(conn, 422, []byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"invalid","field":"head","message":"head branch does not exist"}]}`), keepAlive)
				return
			}
			pl := &Pull{
				Number: r.NextPull, ID: s.store.id(), NodeID: nodeID("PR", s.store.nextID),
				Title: body.Title, Body: body.Body, State: "open",
				Mergeable: true, HeadRef: head, BaseRef: body.Base,
				HeadSHA: headSHA, BaseSHA: baseSHA, User: viewer,
				CreatedAt: s.store.now, UpdatedAt: s.store.now, Draft: body.Draft,
			}
			r.NextPull++
			r.Pulls = append(r.Pulls, pl)
			out, _ := json.Marshal(s.store.pullJSON(r, pl))
			s.respond(conn, 201, out, keepAlive)
		default:
			s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		return
	}
	var pl *Pull
	for _, x := range r.Pulls {
		if x.Number == num {
			pl = x
			break
		}
	}
	if pl == nil {
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		return
	}
	if len(rest) == 1 && m == "GET" {
		out, _ := json.Marshal(s.store.pullJSON(r, pl))
		s.respond(conn, 200, out, keepAlive)
		return
	}
	if len(rest) == 1 && m == "PATCH" {
		var body struct {
			Title *string `json:"title"`
			Body  *string `json:"body"`
			State *string `json:"state"`
		}
		if json.Unmarshal(req.body, &body) == nil {
			if body.Title != nil {
				pl.Title = *body.Title
			}
			if body.Body != nil {
				pl.Body = *body.Body
			}
			if body.State != nil {
				pl.State = *body.State
				if *body.State == "closed" {
					pl.ClosedAt = &s.store.now
				}
			}
		}
		out, _ := json.Marshal(s.store.pullJSON(r, pl))
		s.respond(conn, 200, out, keepAlive)
		return
	}
	if len(rest) == 2 && rest[1] == "files" && m == "GET" {
		files := s.store.pullFiles(r, pl)
		out, _ := json.Marshal(files)
		s.respond(conn, 200, out, keepAlive)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && m == "PUT" {
		s.mergePull(conn, req, keepAlive, r, pl)
		return
	}
	s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
}

// changedPaths lists file paths whose blob differs between two commit trees.
func (s *Store) changedPaths(r *Repo, from, to string) map[string]bool {
	out := make(map[string]bool)
	var walk func(t1, t2, prefix string)
	filesOf := func(tree string) map[string]treeEntry {
		m := make(map[string]treeEntry)
		o := s.object(r, tree)
		if o == nil || o.Type != "tree" {
			return m
		}
		for _, e := range parseTree(o.Data) {
			m[e.name] = e
		}
		return m
	}
	walk = func(t1, t2, prefix string) {
		a, b := filesOf(t1), filesOf(t2)
		for name, eb := range b {
			p := prefix + name
			ea, ok := a[name]
			if eb.mode == "40000" {
				sub := ""
				if ok && ea.mode == "40000" {
					sub = ea.oid
				}
				walk(sub, eb.oid, p+"/")
			} else if !ok || ea.oid != eb.oid {
				out[p] = true
			}
		}
		for name, ea := range a {
			if _, ok := b[name]; !ok {
				if ea.mode == "40000" {
					walk(ea.oid, "", prefix+name+"/")
				} else {
					out[prefix+name] = true
				}
			}
		}
	}
	m1, m2 := s.commitMeta(r, from), s.commitMeta(r, to)
	if m1 == nil || m2 == nil {
		return out
	}
	walk(m1.Tree, m2.Tree, "")
	return out
}

func (s *Store) pullFiles(r *Repo, pl *Pull) []any {
	changed := s.changedPaths(r, pl.BaseSHA, pl.HeadSHA)
	out := make([]any, 0, len(changed))
	for p := range changed {
		out = append(out, map[string]any{
			"sha": "", "filename": p, "status": "modified",
			"additions": 1, "deletions": 1, "changes": 2,
		})
	}
	return out
}

func (s *Server) mergePull(conn net.Conn, req *request, keepAlive bool, r *Repo, pl *Pull) {
	if pl.State != "open" || pl.Merged {
		s.respond(conn, 405, []byte(`{"message":"Pull Request is not mergeable","documentation_url":"https://docs.github.com/rest"}`), keepAlive)
		return
	}
	baseRef := "refs/heads/" + pl.BaseRef
	curBase := r.Refs[baseRef]
	headSHA := r.Refs["refs/heads/"+pl.HeadRef]
	if headSHA == "" {
		headSHA = pl.HeadSHA
	}
	if curBase != pl.BaseSHA {
		baseChanged := s.store.changedPaths(r, pl.BaseSHA, curBase)
		headChanged := s.store.changedPaths(r, pl.BaseSHA, headSHA)
		for p := range headChanged {
			if baseChanged[p] {
				s.respond(conn, 405, []byte(`{"message":"Merge conflict","documentation_url":"https://docs.github.com/rest"}`), keepAlive)
				return
			}
		}
	}
	headMeta := s.store.commitMeta(r, headSHA)
	if headMeta == nil {
		s.respond(conn, 405, []byte(`{"message":"Pull Request is not mergeable"}`), keepAlive)
		return
	}
	msg := pl.Title + " (#" + strconv.Itoa(pl.Number) + ")"
	commitData := s.store.commitBytes(headMeta.Tree, []string{curBase}, msg)
	sha := s.store.putObject(r, "commit", commitData)
	r.Refs[baseRef] = sha
	r.PushedAt = s.store.now
	r.fetchCache = nil
	pl.Merged = true
	pl.State = "closed"
	pl.MergeSHA = sha
	pl.ClosedAt = &s.store.now
	pl.MergedAt = &s.store.now
	out, _ := json.Marshal(map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
	s.respond(conn, 200, out, keepAlive)
}

// ---------------------------------------------------------------- contents

func (s *Store) commitBytes(tree string, parents []string, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	ident := "Forge Bot <bot@forge.local> 1700000000 +0000"
	b.WriteString("author " + ident + "\n")
	b.WriteString("committer " + ident + "\n")
	b.WriteString("\n" + message + "\n")
	return []byte(b.String())
}

// buildContentsTree rebuilds the whole tree from the flat contents map.
func (s *Store) buildContentsTree(r *Repo, dir string) string {
	entries := make([]treeEntry, 0)
	subdirs := make(map[string]bool)
	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	for p, c := range r.Contents {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		restPath := p[len(prefix):]
		if slash := strings.IndexByte(restPath, '/'); slash >= 0 {
			subdirs[restPath[:slash]] = true
		} else {
			entries = append(entries, treeEntry{mode: "100644", name: restPath, oid: c.SHA})
		}
	}
	for d := range subdirs {
		sub := dir + "/" + d
		if dir == "" {
			sub = d
		}
		entries = append(entries, treeEntry{mode: "40000", name: d, oid: s.buildContentsTree(r, sub)})
	}
	treeData := buildTreeData(entries)
	return s.putObject(r, "tree", treeData)
}

func (s *Server) contentsRoute(conn net.Conn, req *request, keepAlive bool, r *Repo, rest []string) {
	p := strings.Join(rest, "/")
	m := req.method
	switch m {
	case "GET":
		if c, ok := r.Contents[p]; ok {
			out, _ := json.Marshal(map[string]any{
				"name": path.Base(p), "path": p, "sha": c.SHA, "size": len(c.Data),
				"type": "file", "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString(c.Data),
				"url":     "http://forge.local/api/v3/repos/" + r.FullName + "/contents/" + p,
			})
			s.respond(conn, 200, out, keepAlive)
			return
		}
		// directory listing?
		prefix := p + "/"
		if p == "" {
			prefix = ""
		}
		items := make([]any, 0)
		seen := make(map[string]bool)
		for cp, c := range r.Contents {
			if !strings.HasPrefix(cp, prefix) {
				continue
			}
			restPath := cp[len(prefix):]
			if slash := strings.IndexByte(restPath, '/'); slash >= 0 {
				d := restPath[:slash]
				if !seen[d] {
					seen[d] = true
					items = append(items, map[string]any{"name": d, "path": prefix + d, "type": "dir", "sha": ""})
				}
			} else {
				items = append(items, map[string]any{
					"name": restPath, "path": cp, "sha": c.SHA, "size": len(c.Data), "type": "file",
				})
			}
		}
		if len(items) == 0 && p != "" {
			s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
			return
		}
		out, _ := json.Marshal(items)
		s.respond(conn, 200, out, keepAlive)
	case "PUT":
		var body struct {
			Message string `json:"message"`
			Content string `json:"content"`
			SHA     string `json:"sha"`
			Branch  string `json:"branch"`
		}
		if err := json.Unmarshal(req.body, &body); err != nil || body.Message == "" {
			s.respond(conn, 422, []byte(`{"message":"Validation Failed"}`), keepAlive)
			return
		}
		data, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			s.respond(conn, 400, []byte(`{"message":"Invalid base64 content"}`), keepAlive)
			return
		}
		existing, exists := r.Contents[p]
		if exists && body.SHA != existing.SHA {
			s.respond(conn, 409, []byte(`{"message":"Conflict: the file sha does not match the current contents"}`), keepAlive)
			return
		}
		blobOID := s.store.putObject(r, "blob", data)
		r.Contents[p] = &Content{Path: p, SHA: blobOID, Data: data}
		branch := body.Branch
		if branch == "" {
			branch = r.DefaultBranch
		}
		ref := "refs/heads/" + branch
		parent := r.Refs[ref]
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		tree := s.store.buildContentsTree(r, "")
		commitData := s.store.commitBytes(tree, parents, body.Message)
		sha := s.store.putObject(r, "commit", commitData)
		r.Refs[ref] = sha
		r.PushedAt = s.store.now
		r.fetchCache = nil
		status := 200
		if !exists {
			status = 201
		}
		out, _ := json.Marshal(map[string]any{
			"content": map[string]any{"name": path.Base(p), "path": p, "sha": blobOID, "size": len(data), "type": "file"},
			"commit":  map[string]any{"sha": sha, "message": body.Message},
		})
		s.respond(conn, status, out, keepAlive)
	case "DELETE":
		var body struct {
			Message string `json:"message"`
			SHA     string `json:"sha"`
		}
		json.Unmarshal(req.body, &body)
		c, ok := r.Contents[p]
		if !ok {
			s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
			return
		}
		if body.SHA != "" && body.SHA != c.SHA {
			s.respond(conn, 409, []byte(`{"message":"Conflict"}`), keepAlive)
			return
		}
		delete(r.Contents, p)
		ref := "refs/heads/" + r.DefaultBranch
		parent := r.Refs[ref]
		var parents []string
		if parent != "" {
			parents = []string{parent}
		}
		tree := s.store.buildContentsTree(r, "")
		commitData := s.store.commitBytes(tree, parents, body.Message)
		sha := s.store.putObject(r, "commit", commitData)
		r.Refs[ref] = sha
		r.fetchCache = nil
		out, _ := json.Marshal(map[string]any{"content": nil, "commit": map[string]any{"sha": sha}})
		s.respond(conn, 200, out, keepAlive)
	default:
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
	}
}
