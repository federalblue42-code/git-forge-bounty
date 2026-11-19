// Git smart HTTP: info/refs advertisements (v0 + v2), ls-refs, upload-pack
// (fetch/clone, shallow support) and receive-pack (push, report-status).
// Packs are parsed and built by hand on compress/zlib; objects live in the store.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

// ---------------------------------------------------------------- pkt-line

func pkt(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 4, n)
	const hexd = "0123456789abcdef"
	out[0] = hexd[(n>>12)&15]
	out[1] = hexd[(n>>8)&15]
	out[2] = hexd[(n>>4)&15]
	out[3] = hexd[n&15]
	return append(out, payload...)
}

func pktStr(s string) []byte { return pkt([]byte(s)) }

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

// decodePkts splits a pkt-line stream; returns data payloads and the offset
// just past the last flush (trailing bytes, e.g. a pack, are left over).
func decodePkts(b []byte) (lines [][]byte, rest []byte) {
	for len(b) >= 4 {
		n, err := strconv.ParseUint(string(b[:4]), 16, 16)
		if err != nil {
			return lines, b
		}
		if n == 0 {
			return lines, b[4:]
		}
		if n == 1 { // delim
			lines = append(lines, nil)
			b = b[4:]
			continue
		}
		if n < 4 || int(n) > len(b) {
			return lines, b
		}
		lines = append(lines, b[4:n])
		b = b[n:]
	}
	return lines, b
}

// ---------------------------------------------------------------- pack read

// oneByteReader feeds zlib one byte per Read so the stream boundary is exact
// (compress/flate buffers its input; chunked reads would overshoot).
type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func inflateAt(data []byte, off int) ([]byte, int, error) {
	r := &oneByteReader{data: data, pos: off}
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	out, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	return out, r.pos, nil
}

type packObject struct {
	oid  string
	typ  string
	data []byte
}

func typeNum(t string) int {
	switch t {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 0
}

func numType(n int) string {
	switch n {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

// readPack parses a packfile, resolving ofs/ref deltas. baseLookup resolves
// ref-delta bases the pack itself does not carry (thin packs).
func readPack(pack []byte, baseLookup func(string) *Object) ([]packObject, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(sum[:], pack[len(pack)-20:]) {
		return nil, fmt.Errorf("pack checksum mismatch")
	}
	off := 12
	byOffset := make(map[int]packObject, count)
	out := make([]packObject, 0, count)
	for i := uint32(0); i < count; i++ {
		start := off
		b := pack[off]
		off++
		typ := int((b >> 4) & 7)
		size := int(b & 15)
		shift := 4
		for b&0x80 != 0 {
			b = pack[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		var baseOID string
		var baseOff int
		switch typ {
		case 6: // ofs-delta
			b = pack[off]
			off++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[off]
				off++
				v = ((v + 1) << 7) | int(b&0x7f)
			}
			baseOff = start - v
		case 7: // ref-delta
			baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		}
		data, next, err := inflateAt(pack, off)
		if err != nil {
			return nil, fmt.Errorf("object %d: %w", i, err)
		}
		off = next
		if len(data) != size {
			return nil, fmt.Errorf("object %d: size mismatch", i)
		}
		var obj packObject
		if typ == 6 || typ == 7 {
			var base *Object
			if typ == 6 {
				bo, ok := byOffset[baseOff]
				if !ok {
					return nil, fmt.Errorf("ofs-delta base missing")
				}
				base = &Object{Type: bo.typ, Data: bo.data}
			} else {
				for _, o := range out {
					if o.oid == baseOID {
						base = &Object{Type: o.typ, Data: o.data}
						break
					}
				}
				if base == nil && baseLookup != nil {
					base = baseLookup(baseOID)
				}
				if base == nil {
					return nil, fmt.Errorf("ref-delta base %s missing", baseOID)
				}
			}
			resolved, err := applyDelta(base.Data, data)
			if err != nil {
				return nil, err
			}
			obj = packObject{typ: base.Type, data: resolved}
		} else {
			obj = packObject{typ: numType(typ), data: data}
		}
		if obj.typ == "" {
			return nil, fmt.Errorf("bad object type %d", typ)
		}
		obj.oid = hashObject(obj.typ, obj.data)
		byOffset[start] = obj
		out = append(out, obj)
	}
	return out, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() int {
		v := 0
		shift := 0
		for {
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			if b&0x80 == 0 {
				break
			}
			shift += 7
		}
		return v
	}
	srcSize := readVarint()
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	dstSize := readVarint()
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			if op&0x01 != 0 {
				cpOff = int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				cpOff |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				cpOff |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				cpOff |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				cpSize = int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				cpSize |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				cpSize |= int(delta[pos]) << 16
				pos++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op > 0 {
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("bad delta opcode 0")
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("delta result size mismatch")
	}
	return out, nil
}

// ---------------------------------------------------------------- pack write

func deflateBytes(data []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(data)
	zw.Close()
	return buf.Bytes()
}

// writePack builds a plain (non-delta) packfile from objects in order.
func writePack(objects []packObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	binary.Write(&buf, binary.BigEndian, uint32(2))
	binary.Write(&buf, binary.BigEndian, uint32(len(objects)))
	for _, o := range objects {
		size := len(o.data)
		b := byte(typeNum(o.typ)<<4 | (size & 15))
		size >>= 4
		for size > 0 {
			buf.WriteByte(b | 0x80)
			b = byte(size & 0x7f)
			size >>= 7
		}
		buf.WriteByte(b)
		buf.Write(deflateBytes(o.data))
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// ---------------------------------------------------------------- commit/tree helpers

func parseCommit(data []byte) (tree string, parents []string, rest string) {
	lines := strings.Split(string(data), "\n")
	i := 0
	for ; i < len(lines); i++ {
		l := lines[i]
		if l == "" {
			break
		}
		if strings.HasPrefix(l, "tree ") {
			tree = l[5:]
		} else if strings.HasPrefix(l, "parent ") {
			parents = append(parents, l[7:])
		}
	}
	if i+1 < len(lines) {
		rest = strings.Join(lines[i+1:], "\n")
	}
	return
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+21 {
			break
		}
		name := string(data[:nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		data = data[nul+21:]
		out = append(out, treeEntry{mode, name, oid})
	}
	return out
}

func buildTreeData(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].name, sorted[j].name
		if sorted[i].mode == "40000" {
			a += "/"
		}
		if sorted[j].mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// commitMeta caches a parsed commit.
func (s *Store) commitMeta(r *Repo, oid string) *CommitMeta {
	if m, ok := r.Commits[oid]; ok {
		return m
	}
	o := s.object(r, oid)
	if o == nil || o.Type != "commit" {
		return nil
	}
	tree, parents, msg := parseCommit(o.Data)
	m := &CommitMeta{SHA: oid, Tree: tree, Parents: parents, Message: msg}
	r.Commits[oid] = m
	return m
}

// reachable collects oids reachable from roots, excluding stop oids, optionally
// bounded by commit depth. Returns objects and shallow boundary commits.
func (s *Store) reachable(r *Repo, roots []string, stop map[string]bool, depth int) ([]packObject, []string) {
	seen := make(map[string]bool)
	var objects []packObject
	var shallow []string
	type item struct {
		oid string
		d   int
	}
	queue := make([]item, 0, len(roots))
	for _, root := range roots {
		queue = append(queue, item{root, 1})
	}
	var addObject func(oid string)
	addObject = func(oid string) {
		if seen[oid] || stop[oid] {
			return
		}
		seen[oid] = true
		o := s.object(r, oid)
		if o == nil {
			return
		}
		objects = append(objects, packObject{oid: oid, typ: o.Type, data: o.Data})
		if o.Type == "tree" {
			for _, e := range parseTree(o.Data) {
				addObject(e.oid)
			}
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.oid] || stop[it.oid] {
			continue
		}
		m := s.commitMeta(r, it.oid)
		if m == nil {
			addObject(it.oid)
			continue
		}
		seen[it.oid] = true
		o := s.object(r, it.oid)
		objects = append(objects, packObject{oid: it.oid, typ: "commit", data: o.Data})
		addObject(m.Tree)
		if depth > 0 && it.d >= depth {
			if len(m.Parents) > 0 {
				shallow = append(shallow, it.oid)
			}
			continue
		}
		for _, p := range m.Parents {
			queue = append(queue, item{p, it.d + 1})
		}
	}
	return objects, shallow
}

// inflatePacks inflates every stored pack into the object cache (lazy push path).
func (s *Store) inflatePacks(r *Repo) {
	for oid, pack := range r.Packs {
		objs, err := readPack(pack, func(oid string) *Object { return r.Objects[oid] })
		if err != nil {
			continue
		}
		for _, o := range objs {
			if _, ok := r.Objects[o.oid]; !ok {
				r.Objects[o.oid] = &Object{Type: o.typ, Data: o.data}
			}
		}
		delete(r.Packs, oid)
	}
}
