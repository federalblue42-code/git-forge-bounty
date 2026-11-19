// The git smart-HTTP routes: /{owner}/{repo}.git/info/refs, git-upload-pack and
// git-receive-pack. upload-pack speaks v2 when asked (Git-Protocol: version=2)
// and v0 otherwise; receive-pack is v0 with report-status + side-band-64k.
package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
)

func (s *Server) gitRoute(conn net.Conn, req *request, keepAlive bool) {
	// path: /{owner}/{repo}.git/{service...}
	p := strings.TrimPrefix(req.path, "/")
	idx := strings.Index(p, ".git")
	if idx < 0 {
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		return
	}
	ownerRepo := p[:idx]
	slash := strings.IndexByte(ownerRepo, '/')
	if slash < 0 {
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		return
	}
	owner, name := ownerRepo[:slash], ownerRepo[slash+1:]
	sub := p[idx+4:] // "/info/refs" | "/git-upload-pack" | "/git-receive-pack"

	if !s.authed(req) {
		s.respond(conn, 401, []byte(`{"message":"Bad credentials"}`), keepAlive)
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.repo(owner, name)
	if r == nil {
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
		return
	}

	switch {
	case sub == "/info/refs":
		q := parseQuery(req.query)
		service := q["service"]
		if service != "git-upload-pack" && service != "git-receive-pack" {
			s.respond(conn, 403, []byte(`{"message":"Invalid service"}`), keepAlive)
			return
		}
		v2 := service == "git-upload-pack" && strings.Contains(req.get("Git-Protocol"), "version=2")
		body := s.advertisement(r, service, v2)
		s.gitRespond(conn, 200, "application/x-"+service+"-advertisement", body, keepAlive)
	case sub == "/git-upload-pack" && req.method == "POST":
		v2 := strings.Contains(req.get("Git-Protocol"), "version=2")
		s.uploadPack(conn, req, keepAlive, r, v2)
	case sub == "/git-receive-pack" && req.method == "POST":
		s.receivePack(conn, req, keepAlive, r)
	default:
		s.respond(conn, 404, []byte(`{"message":"Not Found"}`), keepAlive)
	}
}

func (s *Server) gitRespond(conn net.Conn, status int, ctype string, body []byte, keepAlive bool) {
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 ")
	b.WriteString(statusText(status))
	b.WriteString("\r\nContent-Type: ")
	b.WriteString(ctype)
	b.WriteString("\r\nCache-Control: no-cache\r\nX-GitHub-Request-Id: ")
	b.WriteString(strconv.Itoa(int(s.reqID.Add(1))))
	b.WriteString("\r\nContent-Length: ")
	b.WriteString(strconv.Itoa(len(body)))
	if keepAlive {
		b.WriteString("\r\nConnection: keep-alive")
	} else {
		b.WriteString("\r\nConnection: close")
	}
	b.WriteString("\r\n\r\n")
	b.Write(body)
	conn.Write(b.Bytes())
}

const agentCap = "agent=git/2.47.0"

func (s *Server) advertisement(r *Repo, service string, v2 bool) []byte {
	var b bytes.Buffer
	if v2 {
		b.Write(pktStr("version 2\n"))
		b.Write(pktStr(agentCap + "\n"))
		b.Write(pktStr("ls-refs\n"))
		b.Write(pktStr("fetch=shallow\n"))
		b.Write(pktStr("server-option\n"))
		b.Write(pktStr("object-format=sha1\n"))
		b.Write(flushPkt)
		return b.Bytes()
	}
	b.Write(pktStr("# service=" + service + "\n"))
	b.Write(flushPkt)
	caps := "multi_ack_detailed side-band-64k ofs-delta shallow thin-pack no-progress " + agentCap + " object-format=sha1"
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs ofs-delta quiet " + agentCap + " object-format=sha1"
	}
	refs := sortedRefs(r)
	if len(refs) == 0 {
		b.Write(pktStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		b.Write(flushPkt)
		return b.Bytes()
	}
	headOID := r.Refs["refs/heads/"+r.DefaultBranch]
	first := true
	if headOID != "" {
		line := headOID + " HEAD\x00" + caps + " symref=HEAD:refs/heads/" + r.DefaultBranch + "\n"
		b.Write(pktStr(line))
		first = false
	}
	for _, ref := range refs {
		oid := r.Refs[ref]
		line := oid + " " + ref
		if first {
			line += "\x00" + caps
			first = false
		}
		b.Write(pktStr(line + "\n"))
	}
	b.Write(flushPkt)
	return b.Bytes()
}

func sortedRefs(r *Repo) []string {
	out := make([]string, 0, len(r.Refs))
	for ref := range r.Refs {
		out = append(out, ref)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------- upload-pack

func (s *Server) uploadPack(conn net.Conn, req *request, keepAlive bool, r *Repo, v2 bool) {
	lines, _ := decodePkts(req.body)
	if v2 && len(lines) > 0 && strings.HasPrefix(string(lines[0]), "command=ls-refs") {
		s.lsRefs(conn, keepAlive, r, lines)
		return
	}
	var wants, haves []string
	depth := 0
	for _, ln := range lines {
		if ln == nil {
			continue
		}
		line := string(ln)
		switch {
		case strings.HasPrefix(line, "want "):
			oid := line[5:45]
			wants = append(wants, oid)
		case strings.HasPrefix(line, "have "):
			haves = append(haves, line[5:45])
		case strings.HasPrefix(line, "deepen "):
			depth, _ = strconv.Atoi(strings.TrimSpace(line[7:]))
		}
	}
	if len(wants) == 0 {
		s.gitRespond(conn, 200, "application/x-git-upload-pack-result", flushPkt, keepAlive)
		return
	}
	cacheKey := strings.Join(wants, ",") + "|" + strconv.Itoa(depth) + "|" + strconv.FormatBool(v2) + "|" + strings.Join(haves, ",")
	if r.fetchCache != nil {
		if cached, ok := r.fetchCache[cacheKey]; ok {
			s.gitRespond(conn, 200, "application/x-git-upload-pack-result", cached, keepAlive)
			return
		}
	}
	stop := make(map[string]bool, len(haves))
	for _, h := range haves {
		stop[h] = true
	}
	objects, shallow := s.store.reachable(r, wants, stop, depth)
	pack := writePack(objects)

	var b bytes.Buffer
	if v2 {
		b.Write(pktStr("acknowledgments\n"))
		b.Write(pktStr("NAK\n"))
		b.Write(flushPkt)
		if len(shallow) > 0 {
			b.Write(pktStr("shallow-info\n"))
			for _, sh := range shallow {
				b.Write(pktStr("shallow " + sh + "\n"))
			}
			b.Write(flushPkt)
		}
		b.Write(pktStr("packfile\n"))
		writeSideband(&b, pack)
		b.Write(flushPkt)
	} else {
		for _, sh := range shallow {
			b.Write(pktStr("shallow " + sh + "\n"))
		}
		b.Write(pktStr("NAK\n"))
		writeSideband(&b, pack)
		b.Write(flushPkt)
	}
	resp := b.Bytes()
	if r.fetchCache == nil {
		r.fetchCache = make(map[string][]byte)
	}
	r.fetchCache[cacheKey] = resp
	s.gitRespond(conn, 200, "application/x-git-upload-pack-result", resp, keepAlive)
}

// lsRefs answers the v2 ls-refs command: one "<oid> <ref>" line per ref, HEAD
// carrying its symref attribute, then a flush.
func (s *Server) lsRefs(conn net.Conn, keepAlive bool, r *Repo, lines [][]byte) {
	symrefs := false
	var prefixes []string
	for _, ln := range lines {
		if ln == nil {
			continue
		}
		line := strings.TrimSuffix(string(ln), "\n")
		if line == "symrefs" {
			symrefs = true
		} else if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, line[11:])
		}
	}
	var b bytes.Buffer
	refs := sortedRefs(r)
	headTarget := "refs/heads/" + r.DefaultBranch
	headOID := r.Refs[headTarget]
	if headOID != "" && matchPrefix("HEAD", prefixes) {
		line := headOID + " HEAD"
		if symrefs {
			line += " symref=HEAD:" + headTarget
		}
		b.Write(pktStr(line + "\n"))
	}
	for _, ref := range refs {
		if !matchPrefix(ref, prefixes) {
			continue
		}
		b.Write(pktStr(r.Refs[ref] + " " + ref + "\n"))
	}
	b.Write(flushPkt)
	s.gitRespond(conn, 200, "application/x-git-upload-pack-result", b.Bytes(), keepAlive)
}

func matchPrefix(ref string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// writeSideband frames pack bytes on band 1, at most 65515 payload bytes a pkt.
func writeSideband(b *bytes.Buffer, pack []byte) {
	const chunk = 65515
	for len(pack) > 0 {
		n := len(pack)
		if n > chunk {
			n = chunk
		}
		payload := make([]byte, 0, n+1)
		payload = append(payload, 1)
		payload = append(payload, pack[:n]...)
		b.Write(pkt(payload))
		pack = pack[n:]
	}
}

// ---------------------------------------------------------------- receive-pack

func (s *Server) receivePack(conn net.Conn, req *request, keepAlive bool, r *Repo) {
	lines, packBytes := decodePkts(req.body)
	type cmd struct{ old, new, ref string }
	var cmds []cmd
	caps := ""
	for _, ln := range lines {
		if ln == nil {
			continue
		}
		line := string(ln)
		if i := strings.IndexByte(line, 0); i >= 0 {
			caps = line[i+1:]
			line = line[:i]
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			cmds = append(cmds, cmd{parts[0], parts[1], parts[2]})
		}
	}
	sideband := strings.Contains(caps, "side-band-64k")

	unpackOK := true
	unpackDetail := "ok"
	var objs []packObject
	if len(packBytes) > 0 {
		var err error
		objs, err = readPack(packBytes, func(oid string) *Object { return s.store.object(r, oid) })
		if err != nil {
			unpackOK = false
			unpackDetail = "invalid pack: " + err.Error()
		}
	}
	if unpackOK {
		for _, o := range objs {
			if _, ok := r.Objects[o.oid]; !ok {
				r.Objects[o.oid] = &Object{Type: o.typ, Data: o.data}
			}
		}
	}

	var report bytes.Buffer
	report.Write(pktStr("unpack " + unpackDetail + "\n"))
	for _, c := range cmds {
		reason := ""
		if !unpackOK {
			reason = "unpacker error"
		} else if c.new == zeroOID {
			if _, ok := r.Refs[c.ref]; !ok {
				reason = "ref not found"
			} else {
				delete(r.Refs, c.ref)
			}
		} else {
			cur := r.Refs[c.ref]
			if c.old != cur && !(c.old == zeroOID && cur == "") {
				reason = "non-fast-forward"
			} else if cur != "" && c.old == cur {
				// ancestry check: new must descend from old
				if !s.store.descends(r, c.new, c.old) {
					reason = "non-fast-forward"
				}
			}
			if reason == "" {
				r.Refs[c.ref] = c.new
				r.PushedAt = s.store.now
				r.fetchCache = nil
			}
		}
		if reason == "" {
			report.Write(pktStr("ok " + c.ref + "\n"))
		} else {
			report.Write(pktStr("ng " + c.ref + " " + reason + "\n"))
		}
	}
	report.Write(flushPkt)

	var b bytes.Buffer
	if sideband {
		writeSideband(&b, report.Bytes())
		b.Write(flushPkt)
	} else {
		b.Write(report.Bytes())
	}
	s.gitRespond(conn, 200, "application/x-git-receive-pack-result", b.Bytes(), keepAlive)
}

// descends reports whether oid has ancestor among its commit parents (BFS).
func (s *Store) descends(r *Repo, oid, ancestor string) bool {
	seen := map[string]bool{}
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == ancestor {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		m := s.commitMeta(r, cur)
		if m == nil {
			continue
		}
		queue = append(queue, m.Parents...)
	}
	return false
}
