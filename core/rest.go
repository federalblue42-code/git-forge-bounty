// REST v3 surface under /api/v3, plus the router that dispatches the four surfaces.
package main

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
)

func (s *Server) route(conn net.Conn, req *request, keepAlive bool) {
	p := req.path
	switch {
	case p == "/healthz":
		s.respond(conn, 200, []byte(`{"ok":true}`), keepAlive)
	case strings.HasPrefix(p, "/api/v3"):
		if !s.authed(req) {
			s.respond(conn, 401, []byte(`{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest"}`), keepAlive)
			return
		}
		s.restRoute(conn, req, keepAlive)
	case p == "/api/graphql":
		if !s.authed(req) {
			s.respond(conn, 401, []byte(`{"message":"Bad credentials"}`), keepAlive)
			return
		}
		s.graphqlRoute(conn, req, keepAlive)
	case p == "/mcp":
		s.mcpRoute(conn, req, keepAlive)
	case strings.Contains(p, ".git"):
		s.gitRoute(conn, req, keepAlive)
	default:
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
	}
}

func (s *Server) authed(req *request) bool {
	auth := req.get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !equalFold(auth[:len(prefix)], prefix) {
		return false
	}
	return auth[len(prefix):] == s.token
}

func segs(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (s *Server) restRoute(conn net.Conn, req *request, keepAlive bool) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	p := strings.TrimPrefix(req.path, "/api/v3")
	p = strings.TrimPrefix(p, "/")
	parts := segs(p)
	m := req.method

	if len(parts) == 0 {
		body, _ := json.Marshal(map[string]any{
			"current_user_url": "http://forge.local/api/v3/user",
			"repository_url":   "http://forge.local/api/v3/repos/{owner}/{repo}",
			"user_url":         "http://forge.local/api/v3/users/{user}",
		})
		s.respond(conn, 200, body, keepAlive, "X-OAuth-Scopes: repo, read:org")
		return
	}

	switch parts[0] {
	case "meta":
		s.respond(conn, 200, []byte(`{"verifiable_password_authentication":false,"installed_version":"3.14.0"}`), keepAlive)
		return
	case "user":
		if len(parts) >= 2 && parts[1] == "repos" {
			if m == "POST" {
				s.createRepo(conn, req, keepAlive)
				return
			}
			s.listUserRepos(conn, req, keepAlive)
			return
		}
		if len(parts) == 1 && m == "GET" {
			body, _ := json.Marshal(userJSON(viewer))
			s.respond(conn, 200, body, keepAlive)
			return
		}
	case "repos":
		if len(parts) >= 3 {
			s.repoRoute(conn, req, keepAlive, parts[1], parts[2], parts[3:])
			return
		}
	}
	s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
}

// ---------------------------------------------------------------- serializers

func userJSON(u *User) map[string]any {
	return map[string]any{
		"login": u.Login, "id": u.ID, "node_id": u.NodeID,
		"avatar_url": "", "gravatar_id": "", "url": "http://forge.local/api/v3/users/" + u.Login,
		"html_url": "http://forge.local/" + u.Login, "type": u.Type, "site_admin": false,
		"name": u.Name, "email": u.Email,
	}
}

func (s *Store) repoJSON(r *Repo) map[string]any {
	base := "http://forge.local/" + r.FullName
	api := "http://forge.local/api/v3/repos/" + r.FullName
	return map[string]any{
		"id": r.ID, "node_id": r.NodeID, "name": r.Name, "full_name": r.FullName,
		"private": r.Private, "owner": userJSON(viewer),
		"html_url": base, "description": r.Description, "fork": false,
		"url": api, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "pushed_at": r.PushedAt,
		"git_url":   "git://forge.local/" + r.FullName + ".git",
		"ssh_url":   "git@forge.local:" + r.FullName + ".git",
		"clone_url": base + ".git", "svn_url": base,
		"homepage": nil, "size": 0, "stargazers_count": 0, "watchers_count": 0,
		"language": nil, "has_issues": true, "has_projects": true, "has_downloads": true,
		"has_wiki": true, "has_pages": false, "forks_count": 0, "mirror_url": nil,
		"archived": false, "disabled": false, "open_issues_count": len(r.Issues),
		"license": nil, "allow_forking": true, "is_template": false,
		"topics": []string{}, "visibility": "public", "forks": 0,
		"open_issues": len(r.Issues), "watchers": 0, "default_branch": r.DefaultBranch,
		"permissions": map[string]any{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
	}
}

func (s *Store) issueJSON(r *Repo, it *Issue) map[string]any {
	labels := make([]any, 0, len(it.Labels))
	for _, l := range it.Labels {
		labels = append(labels, labelJSON(l))
	}
	api := "http://forge.local/api/v3/repos/" + r.FullName
	return map[string]any{
		"url":            api + "/issues/" + strconv.Itoa(it.Number),
		"repository_url": api,
		"labels_url":     api + "/issues/" + strconv.Itoa(it.Number) + "/labels{/name}",
		"comments_url":   api + "/issues/" + strconv.Itoa(it.Number) + "/comments",
		"events_url":     api + "/issues/" + strconv.Itoa(it.Number) + "/events",
		"html_url":       "http://forge.local/" + r.FullName + "/issues/" + strconv.Itoa(it.Number),
		"id":             it.ID, "node_id": it.NodeID, "number": it.Number,
		"title": it.Title, "body": it.Body, "state": it.State, "locked": it.Locked,
		"user": userJSON(it.User), "labels": labels,
		"assignee": nil, "assignees": []any{}, "milestone": nil,
		"comments": it.Comments, "created_at": it.CreatedAt, "updated_at": it.UpdatedAt,
		"closed_at": it.ClosedAt, "author_association": "OWNER", "state_reason": nil,
	}
}

func labelJSON(l *Label) map[string]any {
	return map[string]any{
		"id": l.ID, "node_id": l.NodeID, "url": l.URL, "name": l.Name,
		"color": l.Color, "default": l.Default, "description": l.Description,
	}
}

func (s *Store) pullJSON(r *Repo, pl *Pull) map[string]any {
	api := "http://forge.local/api/v3/repos/" + r.FullName
	n := strconv.Itoa(pl.Number)
	return map[string]any{
		"url": api + "/pulls/" + n, "id": pl.ID, "node_id": pl.NodeID,
		"html_url": "http://forge.local/" + r.FullName + "/pull/" + n,
		"number":   pl.Number, "state": pl.State, "locked": false,
		"title": pl.Title, "body": pl.Body, "user": userJSON(pl.User),
		"created_at": pl.CreatedAt, "updated_at": pl.UpdatedAt,
		"closed_at": pl.ClosedAt, "merged_at": pl.MergedAt,
		"draft": pl.Draft, "merged": pl.Merged, "mergeable": pl.Mergeable,
		"merge_commit_sha": pl.MergeSHA,
		"head":             map[string]any{"label": r.FullName + ":" + pl.HeadRef, "ref": pl.HeadRef, "sha": pl.HeadSHA},
		"base":             map[string]any{"label": r.FullName + ":" + pl.BaseRef, "ref": pl.BaseRef, "sha": pl.BaseSHA},
	}
}

// ---------------------------------------------------------------- handlers

func (s *Server) createRepo(conn net.Conn, req *request, keepAlive bool) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
		AutoInit    bool   `json:"auto_init"`
	}
	if err := json.Unmarshal(req.body, &body); err != nil || body.Name == "" {
		s.respond(conn, 422, []byte(`{"message":"Validation Failed","errors":[{"resource":"Repository","code":"custom","field":"name","message":"name is required"}]}`), keepAlive)
		return
	}
	r, ok := s.store.createRepo(body.Name, body.Description, body.Private)
	if !ok {
		s.respond(conn, 422, []byte(`{"message":"Repository creation failed.","errors":[{"resource":"Repository","code":"custom","field":"name","message":"name already exists on this account"}]}`), keepAlive)
		return
	}
	out, _ := json.Marshal(s.store.repoJSON(r))
	s.respond(conn, 201, out, keepAlive)
}

func (s *Server) listUserRepos(conn net.Conn, req *request, keepAlive bool) {
	q := parseQuery(req.query)
	perPage := 30
	if v := q["per_page"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			perPage = n
		}
	}
	page := 1
	if v := q["page"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	all := s.store.repoList
	// newest first, deterministic
	list := make([]*Repo, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		list = append(list, all[i])
	}
	total := len(list)
	lastPage := (total + perPage - 1) / perPage
	if lastPage == 0 {
		lastPage = 1
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	items := make([]any, 0, end-start)
	for _, r := range list[start:end] {
		items = append(items, s.store.repoJSON(r))
	}
	out, _ := json.Marshal(items)
	extra := []string{}
	if page < lastPage {
		extra = append(extra, `Link: <http://forge.local/api/v3/user/repos?per_page=`+strconv.Itoa(perPage)+`&page=`+strconv.Itoa(page+1)+`>; rel="next", <http://forge.local/api/v3/user/repos?per_page=`+strconv.Itoa(perPage)+`&page=`+strconv.Itoa(lastPage)+`>; rel="last"`)
	}
	s.respond(conn, 200, out, keepAlive, extra...)
}

func parseQuery(q string) map[string]string {
	out := make(map[string]string)
	for _, kv := range strings.Split(q, "&") {
		if kv == "" {
			continue
		}
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			out[kv[:eq]] = kv[eq+1:]
		} else {
			out[kv] = ""
		}
	}
	return out
}

func (s *Server) repoRoute(conn net.Conn, req *request, keepAlive bool, owner, name string, rest []string) {
	r := s.store.repo(owner, name)
	if r == nil {
		s.respond(conn, 404, []byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`), keepAlive)
		return
	}
	m := req.method
	if len(rest) == 0 {
		switch m {
		case "GET":
			out, _ := json.Marshal(s.store.repoJSON(r))
			s.respond(conn, 200, out, keepAlive)
		case "PATCH":
			var body struct {
				Description *string `json:"description"`
				Private     *bool   `json:"private"`
			}
			if json.Unmarshal(req.body, &body) == nil {
				if body.Description != nil {
					r.Description = *body.Description
				}
				if body.Private != nil {
					r.Private = *body.Private
				}
			}
			out, _ := json.Marshal(s.store.repoJSON(r))
			s.respond(conn, 200, out, keepAlive)
		case "DELETE":
			s.store.deleteRepo(owner, name)
			s.respond(conn, 204, nil, keepAlive)
		default:
			s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		}
		return
	}
	switch rest[0] {
	case "issues":
		s.issuesRoute(conn, req, keepAlive, r, rest[1:])
	case "labels":
		s.labelsRoute(conn, req, keepAlive, r, rest[1:])
	case "pulls":
		s.pullsRoute(conn, req, keepAlive, r, rest[1:])
	case "contents":
		s.contentsRoute(conn, req, keepAlive, r, rest[1:])
	case "branches":
		out, _ := json.Marshal([]any{})
		s.respond(conn, 200, out, keepAlive)
	default:
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
	}
}

func (s *Server) issuesRoute(conn net.Conn, req *request, keepAlive bool, r *Repo, rest []string) {
	m := req.method
	if len(rest) == 0 {
		switch m {
		case "GET":
			q := parseQuery(req.query)
			state := q["state"]
			if state == "" {
				state = "open"
			}
			items := make([]any, 0)
			for _, it := range r.Issues {
				if state == "all" || it.State == state {
					items = append(items, s.store.issueJSON(r, it))
				}
			}
			out, _ := json.Marshal(items)
			s.respond(conn, 200, out, keepAlive)
		case "POST":
			var body struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(req.body, &body); err != nil || body.Title == "" {
				s.respond(conn, 422, []byte(`{"message":"Validation Failed"}`), keepAlive)
				return
			}
			labels := make([]*Label, 0, len(body.Labels))
			for _, ln := range body.Labels {
				l, ok := r.Labels[ln]
				if !ok {
					s.respond(conn, 422, []byte(`{"message":"Validation Failed","errors":[{"resource":"Issue","code":"invalid","field":"labels","message":"label does not exist"}]}`), keepAlive)
					return
				}
				labels = append(labels, l)
			}
			it := &Issue{
				Number: r.NextIssue, ID: s.store.id(), NodeID: nodeID("I", s.store.nextID),
				Title: body.Title, Body: body.Body, State: "open", Labels: labels,
				User: viewer, CreatedAt: s.store.now, UpdatedAt: s.store.now,
			}
			r.NextIssue++
			r.Issues = append(r.Issues, it)
			out, _ := json.Marshal(s.store.issueJSON(r, it))
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
	var it *Issue
	for _, x := range r.Issues {
		if x.Number == num {
			it = x
			break
		}
	}
	if it == nil {
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		return
	}
	if len(rest) == 1 {
		switch m {
		case "GET":
			out, _ := json.Marshal(s.store.issueJSON(r, it))
			s.respond(conn, 200, out, keepAlive)
		case "PATCH":
			var body struct {
				Title *string `json:"title"`
				Body  *string `json:"body"`
				State *string `json:"state"`
			}
			if json.Unmarshal(req.body, &body) == nil {
				if body.Title != nil {
					it.Title = *body.Title
				}
				if body.Body != nil {
					it.Body = *body.Body
				}
				if body.State != nil {
					it.State = *body.State
					if *body.State == "closed" {
						it.ClosedAt = &s.store.now
					} else {
						it.ClosedAt = nil
					}
				}
			}
			out, _ := json.Marshal(s.store.issueJSON(r, it))
			s.respond(conn, 200, out, keepAlive)
		default:
			s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		}
		return
	}
	if rest[1] == "comments" {
		if m == "GET" {
			s.respond(conn, 200, []byte(`[]`), keepAlive)
			return
		}
		if m == "POST" {
			var body struct {
				Body string `json:"body"`
			}
			json.Unmarshal(req.body, &body)
			it.Comments++
			cid := s.store.id()
			out, _ := json.Marshal(map[string]any{
				"id": cid, "node_id": nodeID("IC", cid), "body": body.Body,
				"user": userJSON(viewer), "created_at": s.store.now, "updated_at": s.store.now,
				"html_url": "http://forge.local/" + r.FullName + "/issues/" + strconv.Itoa(num) + "#issuecomment-" + strconv.FormatInt(cid, 10),
			})
			s.respond(conn, 201, out, keepAlive)
			return
		}
	}
	s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
}

func (s *Server) labelsRoute(conn net.Conn, req *request, keepAlive bool, r *Repo, rest []string) {
	m := req.method
	if len(rest) == 0 {
		switch m {
		case "GET":
			items := make([]any, 0, len(r.Labels))
			for _, l := range r.Labels {
				items = append(items, labelJSON(l))
			}
			out, _ := json.Marshal(items)
			s.respond(conn, 200, out, keepAlive)
		case "POST":
			var body struct {
				Name        string `json:"name"`
				Color       string `json:"color"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal(req.body, &body); err != nil || body.Name == "" {
				s.respond(conn, 422, []byte(`{"message":"Validation Failed"}`), keepAlive)
				return
			}
			if _, exists := r.Labels[body.Name]; exists {
				s.respond(conn, 422, []byte(`{"message":"Validation Failed","errors":[{"resource":"Label","code":"already_exists","field":"name"}]}`), keepAlive)
				return
			}
			l := &Label{ID: s.store.id(), NodeID: nodeID("L", s.store.nextID), Name: body.Name, Color: body.Color, Description: body.Description, URL: "http://forge.local/api/v3/repos/" + r.FullName + "/labels/" + body.Name}
			if l.Color == "" {
				l.Color = "ededed"
			}
			r.Labels[body.Name] = l
			out, _ := json.Marshal(labelJSON(l))
			s.respond(conn, 201, out, keepAlive)
		default:
			s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		}
		return
	}
	s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
}
