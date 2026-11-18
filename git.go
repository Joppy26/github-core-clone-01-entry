package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

func pktLine(data []byte) []byte {
	n := len(data) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	out = append(out, data...)
	return out
}

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// pktLines parses a pkt-line stream, returning the payloads (without the
// length prefix). Stops at a flush (returns flushSeen=true) or end of input.
// A delim line is returned as a zero-length payload with delim=true handling
// by the caller via the returned slice containing a nil entry marker: we
// return delim positions separately.
type pktResult struct {
	lines    [][]byte // payloads; a nil entry means a delim (0001) line
	flush    bool
	consumed int
}

func parsePktLines(b []byte) pktResult {
	var res pktResult
	i := 0
	for i+4 <= len(b) {
		var n int
		_, err := fmt.Sscanf(string(b[i:i+4]), "%04x", &n)
		if err != nil || n < 0 {
			break
		}
		if n == 0 {
			res.flush = true
			i += 4
			break
		}
		if n == 1 {
			res.lines = append(res.lines, nil)
			i += 4
			continue
		}
		if n < 4 || i+n > len(b) {
			break
		}
		res.lines = append(res.lines, b[i+4:i+n])
		i += n
	}
	res.consumed = i
	return res
}

// ---------------------------------------------------------------------------
// Pack parsing (with OFS_DELTA / REF_DELTA, thin packs resolve from store)
// ---------------------------------------------------------------------------

type packEntry struct {
	typ    int // 1 commit, 2 tree, 3 blob, 4 tag
	oid    [20]byte
	data   []byte
	offset int
}

var errPack = fmt.Errorf("invalid pack")

// parsePack parses pack bytes and stores all objects into r.
// Returns the list of object ids in pack order.
func parsePack(r *Repo, data []byte) ([][20]byte, error) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	if binary.BigEndian.Uint32(data[4:8]) != 2 {
		return nil, fmt.Errorf("unsupported pack version")
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	// Trailer is the last 20 bytes; entries live between offset 12 and len-20.
	end := len(data) - 20
	if end < 12 {
		return nil, fmt.Errorf("pack too short")
	}
	pos := 12
	byOffset := map[int][20]byte{}
	byOffsetData := map[int][]byte{}
	byOffsetType := map[int]int{}
	var oids [][20]byte
	// First pass: read all entries, deferring deltas.
	type pending struct {
		offset   int
		typ      int
		data     []byte
		baseOff  int  // ofs-delta
		baseOID  [20]byte
		isRef    bool
	}
	var pend []pending
	for n := 0; n < count; n++ {
		if pos >= end {
			return nil, fmt.Errorf("pack truncated")
		}
		start := pos
		b := data[pos]
		pos++
		typ := int((b >> 4) & 7)
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= end {
				return nil, fmt.Errorf("pack truncated in size")
			}
			b = data[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var baseOff int
		var baseOID [20]byte
		isRef := false
		if typ == 6 { // OFS_DELTA
			if pos >= end {
				return nil, fmt.Errorf("pack truncated in ofs")
			}
			b = data[pos]
			pos++
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= end {
					return nil, fmt.Errorf("pack truncated in ofs")
				}
				b = data[pos]
				pos++
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			baseOff = start - int(off)
		} else if typ == 7 { // REF_DELTA
			if pos+20 > end {
				return nil, fmt.Errorf("pack truncated in ref")
			}
			copy(baseOID[:], data[pos:pos+20])
			pos += 20
			isRef = true
		}
		// zlib stream: inflate, tracking consumed bytes. *bytes.Reader
		// implements io.ByteReader, so the flate reader pulls exactly to
		// the stream end without over-read (see inflateCounted).
		consumed, raw, err := inflateCounted(data[pos:end], size)
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) != size {
			return nil, fmt.Errorf("object size mismatch: got %d want %d", len(raw), size)
		}
		pos += consumed
		switch typ {
		case 1, 2, 3, 4:
			oid := r.putObject(typeName(typ), raw)
			byOffset[start] = oid
			byOffsetData[start] = raw
			byOffsetType[start] = typ
			oids = append(oids, oid)
		case 6, 7:
			pend = append(pend, pending{offset: start, typ: typ, data: raw, baseOff: baseOff, baseOID: baseOID, isRef: isRef})
		default:
			return nil, fmt.Errorf("unknown object type %d", typ)
		}
	}
	// Resolve deltas in order (bases precede deltas in a well-formed pack;
	// loop until all resolved to tolerate out-of-order thin packs).
	for len(pend) > 0 {
		progress := false
		var rest []pending
		for _, p := range pend {
			var baseOID [20]byte
			var baseType int
			var baseData []byte
			if p.isRef {
				baseOID = p.baseOID
				if obj := r.getObject(baseOID); obj != nil {
					baseType = typeNum(obj.Type)
					baseData = obj.Data
				} else if oid, ok := findPackOID(byOffset, baseOID); ok {
					_ = oid
					// base is in this pack by oid: find its offset data
					if d, t, ok2 := findPackData(byOffset, byOffsetData, byOffsetType, baseOID); ok2 {
						baseData, baseType = d, t
					}
				}
			} else {
				baseOID = byOffset[p.baseOff]
				if d, ok := byOffsetData[p.baseOff]; ok {
					baseData = d
					baseType = byOffsetType[p.baseOff]
				}
			}
			if baseData == nil {
				rest = append(rest, p)
				continue
			}
			out, err := applyDelta(baseData, p.data)
			if err != nil {
				return nil, err
			}
			oid := r.putObject(typeName(baseType), out)
			byOffset[p.offset] = oid
			byOffsetData[p.offset] = out
			byOffsetType[p.offset] = baseType
			oids = append(oids, oid)
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("unresolvable delta base")
		}
		pend = rest
	}
	return oids, nil
}

func findPackOID(byOffset map[int][20]byte, want [20]byte) ([20]byte, bool) {
	for _, oid := range byOffset {
		if oid == want {
			return oid, true
		}
	}
	return zeroOID, false
}

func findPackData(byOffset map[int][20]byte, byData map[int][]byte, byType map[int]int, want [20]byte) ([]byte, int, bool) {
	for off, oid := range byOffset {
		if oid == want {
			return byData[off], byType[off], true
		}
	}
	return nil, 0, false
}

func typeName(t int) string {
	switch t {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return "blob"
}

func typeNum(s string) int {
	switch s {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 3
}

// inflateCounted inflates a zlib stream, returning consumed input bytes and
// output. *bytes.Reader implements io.ByteReader, so zlib's flate reader
// pulls exactly to the stream end and br.Len() gives the remainder.
func inflateCounted(data []byte, _ int64) (int, []byte, error) {
	br := bytes.NewReader(data)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return 0, nil, fmt.Errorf("zlib open: %w", err)
	}
	raw, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return 0, nil, fmt.Errorf("zlib read: %w", err)
	}
	return len(data) - br.Len(), raw, nil
}

// applyDelta applies a git delta to base data.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out []byte
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy from base
			var off, size int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta truncated")
					}
					off |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta truncated")
					}
					size |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			n := int(op)
			if pos+n > len(delta) {
				return nil, fmt.Errorf("delta truncated")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if int64(len(out)) != tgtSize {
		return nil, fmt.Errorf("delta result size mismatch")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pack building (no deltas; objects reachable from wants, minus haves)
// ---------------------------------------------------------------------------

// haveReachable returns every object the client already holds, given its
// have oids. A have that is also a shallow boundary (oldShallow) contributes
// only its own commit + tree/blobs — the client does NOT hold its ancestors
// (that is what a deepen request extends past). Any other have contributes
// its full reachable closure (commit, ancestors, trees, blobs).
func haveReachable(r *Repo, haves map[[20]byte]bool, oldShallow map[[20]byte]bool) map[[20]byte]bool {
	out := map[[20]byte]bool{}
	addTree := func(root [20]byte) {
		stack := [][20]byte{root}
		seen := map[[20]byte]bool{}
		for len(stack) > 0 {
			t := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[t] || out[t] {
				continue
			}
			seen[t] = true
			obj := r.getObject(t)
			if obj == nil {
				continue
			}
			out[t] = true
			if obj.Type != "tree" {
				continue
			}
			for _, e := range parseTree(obj.Data) {
				if e.isDir() {
					stack = append(stack, e.OID)
				} else {
					out[e.OID] = true
				}
			}
		}
	}
	for h := range haves {
		obj := r.getObject(h)
		if obj == nil {
			continue
		}
		out[h] = true
		switch obj.Type {
		case "tag":
			target := r.peelToCommit(h)
			out[target] = true
			h = target
			obj = r.getObject(h)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			fallthrough
		case "commit":
			// Walk commits: full ancestry unless this have is a shallow
			// boundary, in which case only the boundary commit itself.
			stack := [][20]byte{h}
			seen := map[[20]byte]bool{}
			for len(stack) > 0 {
				cur := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if seen[cur] {
					continue
				}
				seen[cur] = true
				co := r.getObject(cur)
				if co == nil || co.Type != "commit" {
					continue
				}
				out[cur] = true
				c := parseCommit(co.Data)
				addTree(c.Tree)
				if !oldShallow[h] {
					stack = append(stack, c.Parents...)
				}
			}
		case "tree":
			addTree(h)
		}
	}
	return out
}

// collectPackObjects returns the objects to include in a pack for the given
// wanted commit/tag oids, excluding anything reachable from haveOids, and
// limited to maxDepth commit generations (0 = unlimited). It also returns
// the set of commits included in the pack (for shallow/unshallow logic).
// haveOids are excluded via their reachable closure (see haveReachable);
// traversal still passes THROUGH an excluded commit so a deepen request
// (want == have == shallow boundary) can reach the parent generation.
func collectPackObjects(r *Repo, wants [][20]byte, haves map[[20]byte]bool, maxDepth int, oldShallow map[[20]byte]bool) ([][20]byte, map[[20]byte]bool) {
	excluded := haveReachable(r, haves, oldShallow)
	included := map[[20]byte]bool{}
	var order [][20]byte
	add := func(oid [20]byte) bool {
		if included[oid] || excluded[oid] {
			return false
		}
		if r.getObject(oid) == nil {
			return false
		}
		included[oid] = true
		order = append(order, oid)
		return true
	}
	// Walk commits breadth-first by generation.
	type item struct {
		oid   [20]byte
		depth int
	}
	var queue []item
	for _, w := range wants {
		// Peel tags to commits for traversal, but include the tag itself.
		obj := r.getObject(w)
		if obj == nil {
			continue
		}
		if obj.Type == "tag" {
			if add(w) {
				// find target
				for _, line := range strings.Split(string(obj.Data), "\n") {
					if strings.HasPrefix(line, "object ") {
						if o, ok := parseOIDHex(strings.TrimSpace(line[7:])); ok {
							queue = append(queue, item{o, 1})
						}
						break
					}
				}
			}
			continue
		}
		queue = append(queue, item{w, 1})
	}
	commitSeen := map[[20]byte]bool{}
	includedCommits := map[[20]byte]bool{}
	var trees [][20]byte
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if commitSeen[it.oid] {
			continue
		}
		commitSeen[it.oid] = true
		obj := r.getObject(it.oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		if add(it.oid) {
			includedCommits[it.oid] = true
			c := parseCommit(obj.Data)
			trees = append(trees, c.Tree)
		}
		// Always traverse parents within the depth limit, even when this
		// commit itself is already held: a deepen fetch's want IS a have,
		// and its parent generation is exactly what must be delivered.
		if maxDepth == 0 || it.depth < maxDepth {
			c := parseCommit(obj.Data)
			for _, p := range c.Parents {
				queue = append(queue, item{p, it.depth + 1})
			}
		}
	}
	// Trees (recursive) then blobs.
	var blobQueue [][20]byte
	seenTree := map[[20]byte]bool{}
	for len(trees) > 0 {
		t := trees[0]
		trees = trees[1:]
		if seenTree[t] {
			continue
		}
		seenTree[t] = true
		if !add(t) {
			continue
		}
		for _, e := range r.listTree(t) {
			if e.isDir() {
				trees = append(trees, e.OID)
			} else {
				blobQueue = append(blobQueue, e.OID)
			}
		}
	}
	for _, b := range blobQueue {
		add(b)
	}
	return order, includedCommits
}

// buildPackBytes serializes objects into a pack (version 2, no deltas).
func buildPackBytes(r *Repo, oids [][20]byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(oids)))
	buf.Write(hdr[:])
	for _, oid := range oids {
		obj := r.getObject(oid)
		if obj == nil {
			continue
		}
		typ := typeNum(obj.Type)
		size := len(obj.Data)
		// varint header: first byte: type in bits 4-6, low 4 bits of size.
		var header []byte
		first := byte((typ << 4) | (size & 0x0f))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		header = append(header, first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			header = append(header, b)
		}
		buf.Write(header)
		zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
		zw.Write(obj.Data)
		zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// sideband wraps data into sideband pkt-lines (band 1), each payload
// <= 65516 bytes including the band byte.
func sideband(data []byte) []byte {
	var out []byte
	const maxPayload = 65516
	for len(data) > 0 {
		n := len(data)
		if n > maxPayload-1 {
			n = maxPayload - 1
		}
		chunk := make([]byte, 0, n+5)
		chunk = append(chunk, pktLine(append([]byte{1}, data[:n]...))...)
		out = append(out, chunk...)
		data = data[n:]
	}
	return out
}

// sortedRefNames returns the repo's ref names in sorted order.
func sortedRefNames(r *Repo) []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
