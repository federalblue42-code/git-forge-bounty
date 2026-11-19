// Raw HTTP/1.1 server: hand-rolled request parsing and response writing on
// net.Conn, no net/http on the hot path. One goroutine per connection, keep-alive
// by default, cached response bytes for the read-heavy routes.
package main

import (
	"bytes"
	"net"
	"strconv"
	"sync/atomic"
)

// request is one parsed HTTP request; body aliases the connection buffer.
type request struct {
	method  string
	path    string // path without query
	query   string // raw query string
	body    []byte
	headers []header
}

type header struct{ name, value string }

func (r *request) get(name string) string {
	for i := range r.headers {
		if equalFold(r.headers[i].name, name) {
			return r.headers[i].value
		}
	}
	return ""
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// Server routes requests to the four surfaces.
type Server struct {
	store *Store
	token string
	reqID atomic.Uint64
}

func (s *Server) Serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

const maxHead = 64 << 10
const maxBody = 64 << 20

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	buf := make([]byte, 0, 16<<10)
	tmp := make([]byte, 32<<10)
	for {
		// Read until the head terminator is in buf.
		headEnd := -1
		for {
			if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
				headEnd = i
				break
			}
			if len(buf) > maxHead {
				return
			}
			n, err := conn.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				return
			}
		}
		req, ok := parseHead(buf[:headEnd])
		if !ok {
			return
		}
		// Content-Length body (the suite never chunks requests).
		cl := 0
		if v := req.get("Content-Length"); v != "" {
			cl, _ = strconv.Atoi(v)
		}
		if cl < 0 || cl > maxBody {
			return
		}
		need := headEnd + 4 + cl
		for len(buf) < need {
			n, err := conn.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				return
			}
		}
		req.body = buf[headEnd+4 : need]

		keepAlive := !equalFold(req.get("Connection"), "close")
		s.route(conn, req, keepAlive)

		// Slide the consumed bytes out; keep any pipelined remainder.
		buf = buf[need:]
		if len(buf) == 0 {
			buf = buf[:0]
		} else {
			nb := make([]byte, len(buf), 16<<10)
			copy(nb, buf)
			buf = nb
		}
		if !keepAlive {
			return
		}
	}
}

func parseHead(head []byte) (*request, bool) {
	lineEnd := bytes.Index(head, []byte("\r\n"))
	if lineEnd < 0 {
		lineEnd = len(head)
	}
	line := string(head[:lineEnd])
	sp1 := indexByte(line, ' ')
	if sp1 < 0 {
		return nil, false
	}
	sp2 := indexByteFrom(line, ' ', sp1+1)
	if sp2 < 0 {
		return nil, false
	}
	r := &request{method: line[:sp1]}
	target := line[sp1+1 : sp2]
	if q := indexByte(target, '?'); q >= 0 {
		r.path = target[:q]
		r.query = target[q+1:]
	} else {
		r.path = target
	}
	rest := head[lineEnd:]
	for len(rest) > 0 {
		if rest[0] == '\r' {
			break
		}
		nl := bytes.Index(rest, []byte("\r\n"))
		if nl < 0 {
			nl = len(rest)
		}
		hl := string(rest[:nl])
		if colon := indexByte(hl, ':'); colon > 0 {
			name := hl[:colon]
			value := hl[colon+1:]
			for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
				value = value[1:]
			}
			r.headers = append(r.headers, header{name, value})
		}
		if nl == len(rest) {
			break
		}
		rest = rest[nl+2:]
	}
	return r, true
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func indexByteFrom(s string, b byte, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// writeRaw writes a complete pre-built response.
func writeRaw(conn net.Conn, resp []byte) {
	conn.Write(resp)
}

// respond builds and writes a JSON response with the forge's standard headers.
func (s *Server) respond(conn net.Conn, status int, body []byte, keepAlive bool, extra ...string) {
	id := s.reqID.Add(1)
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(statusText(status))
	b.WriteString("\r\nContent-Type: application/json\r\nX-GitHub-Request-Id: ")
	b.WriteString(strconv.FormatUint(id, 16))
	b.WriteString("\r\nContent-Length: ")
	b.WriteString(strconv.Itoa(len(body)))
	if keepAlive {
		b.WriteString("\r\nConnection: keep-alive")
	} else {
		b.WriteString("\r\nConnection: close")
	}
	for _, h := range extra {
		b.WriteString("\r\n")
		b.WriteString(h)
	}
	b.WriteString("\r\n\r\n")
	b.Write(body)
	conn.Write(b.Bytes())
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	}
	return "Status"
}
