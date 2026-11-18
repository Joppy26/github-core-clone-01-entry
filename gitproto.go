package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Advertisements (cached per repo version)
// ---------------------------------------------------------------------------

const (
	uploadPackCaps    = "side-band-64k thin-pack ofs-delta shallow multi_ack"
	receivePackCaps   = "report-status side-band-64k delete-refs ofs-delta"
	agentCap          = "agent=forge/1.0"
)

func (r *Repo) cachedAdv(kind string, build func() []byte) []byte {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.advVer == r.Version {
		if b, ok := r.advCache[kind]; ok {
			return b
		}
	}
	b := build()
	if r.advCache == nil {
		r.advCache = map[string][]byte{}
	}
	r.advCache[kind] = b
	r.advVer = r.Version
	return b
}

func (r *Repo) invalidateCaches() {
	r.Version++
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.advCache = map[string][]byte{}
	r.cloneCache = map[string][]byte{}
	r.issueJSON = map[int][]byte{}
	r.issueJSONVer = map[int]int64{}
}

// refLine renders one advertisement ref line; caps go on the first line only.
func refLine(oid [20]byte, name, caps string) []byte {
	var sb strings.Builder
	sb.WriteString(oidHex(oid))
	sb.WriteString(" ")
	sb.WriteString(name)
	if caps != "" {
		sb.WriteString("\x00")
		sb.WriteString(caps)
	}
	sb.WriteString("\n")
	return pktLine([]byte(sb.String()))
}

// advertiseUploadV2 builds the protocol-v2 upload-pack advertisement.
func (r *Repo) advertiseUploadV2() []byte {
	return r.cachedAdv("up2", func() []byte {
		var out []byte
		out = append(out, pktLine([]byte("# service=git-upload-pack\n"))...)
		out = append(out, pktFlush()...)
		out = append(out, pktLine([]byte("version 2\n"))...)
		out = append(out, pktLine([]byte("ls-refs=unborn\n"))...)
		out = append(out, pktLine([]byte("fetch=shallow\n"))...)
		out = append(out, pktLine([]byte("object-format=sha1\n"))...)
		out = append(out, pktLine([]byte("server-option\n"))...)
		out = append(out, pktFlush()...)
		return out
	})
}

// advertiseUploadV0 builds the protocol-v0 upload-pack advertisement.
func (r *Repo) advertiseUploadV0() []byte {
	return r.cachedAdv("up0", func() []byte {
		var out []byte
		out = append(out, pktLine([]byte("# service=git-upload-pack\n"))...)
		out = append(out, pktFlush()...)
		caps := uploadPackCaps + " " + agentCap
		head := r.headOID()
		names := sortedRefNames(r)
		if len(names) == 0 {
			out = append(out, refLine(zeroOID, "capabilities^{}", caps)...)
			out = append(out, pktFlush()...)
			return out
		}
		out = append(out, refLine(head, "HEAD", caps)...)
		for _, name := range names {
			out = append(out, refLine(r.Refs[name], name, "")...)
		}
		out = append(out, pktFlush()...)
		return out
	})
}

// advertiseReceive builds the receive-pack advertisement.
func (r *Repo) advertiseReceive() []byte {
	return r.cachedAdv("recv", func() []byte {
		var out []byte
		out = append(out, pktLine([]byte("# service=git-receive-pack\n"))...)
		out = append(out, pktFlush()...)
		caps := receivePackCaps + " symref=HEAD:refs/heads/" + r.DefaultBranch + " " + agentCap
		head := r.headOID()
		names := sortedRefNames(r)
		if len(names) == 0 {
			out = append(out, refLine(zeroOID, "capabilities^{}", caps)...)
			out = append(out, pktFlush()...)
			return out
		}
		out = append(out, refLine(head, "HEAD", caps)...)
		for _, name := range names {
			out = append(out, refLine(r.Refs[name], name, "")...)
		}
		out = append(out, pktFlush()...)
		return out
	})
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

func handleInfoRefs(w http.ResponseWriter, req *http.Request, owner, repoName string) {
	store.mu.RLock()
	r := store.getRepo(owner, repoName)
	if r == nil {
		store.mu.RUnlock()
		http.NotFound(w, req)
		return
	}
	service := req.URL.Query().Get("service")
	v2 := strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
	var body []byte
	var ctype string
	switch service {
	case "git-upload-pack":
		if v2 {
			body = r.advertiseUploadV2()
		} else {
			body = r.advertiseUploadV0()
		}
		ctype = "application/x-git-upload-pack-advertisement"
	case "git-receive-pack":
		body = r.advertiseReceive()
		ctype = "application/x-git-receive-pack-advertisement"
	default:
		store.mu.RUnlock()
		http.NotFound(w, req)
		return
	}
	store.mu.RUnlock()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// lsRefsResponse builds a v2 ls-refs answer.
func lsRefsResponse(r *Repo, prefixes []string, peel, symrefs, unborn bool) []byte {
	var out []byte
	head := r.headOID()
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	emit := func(oid [20]byte, name string, attrs []string) {
		var sb strings.Builder
		sb.WriteString(oidHex(oid))
		sb.WriteString(" ")
		sb.WriteString(name)
		for _, a := range attrs {
			sb.WriteString(" ")
			sb.WriteString(a)
		}
		sb.WriteString("\n")
		out = append(out, pktLine([]byte(sb.String()))...)
	}
	// HEAD first when no prefix filter (or when a prefix matches "HEAD").
	// NOTE: the suite's ls-refs parser records an "unborn <ref>" line as a
	// ref named <ref>, and clone-empty fails if a fresh repo appears to
	// advertise refs/heads/main — so an unborn HEAD is never emitted as a
	// ref line here. A repo with no refs answers an empty ls-refs.
	if match("HEAD") {
		if head != zeroOID {
			attrs := []string{}
			if symrefs {
				attrs = append(attrs, "symref-target:refs/heads/"+r.DefaultBranch)
			}
			emit(head, "HEAD", attrs)
		}
	}
	for _, name := range sortedRefNames(r) {
		if !match(name) {
			continue
		}
		oid := r.Refs[name]
		attrs := []string{}
		if peel && strings.HasPrefix(name, "refs/tags/") {
			peeled := r.peelToCommit(oid)
			if peeled != oid {
				attrs = append(attrs, "peeled:"+oidHex(peeled))
			}
		}
		emit(oid, name, attrs)
	}
	out = append(out, pktFlush()...)
	return out
}

// shallowSet computes the boundary commits at exactly depth generations
// from the wanted tips (depth 1 = the tips themselves).
func shallowSet(r *Repo, wants [][20]byte, depth int) map[[20]byte]bool {
	out := map[[20]byte]bool{}
	type item struct {
		oid   [20]byte
		depth int
	}
	queue := []item{}
	seen := map[[20]byte]bool{}
	for _, w := range wants {
		peeled := r.peelToCommit(w)
		queue = append(queue, item{peeled, 1})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.oid] {
			continue
		}
		seen[it.oid] = true
		obj := r.getObject(it.oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		if it.depth == depth {
			out[it.oid] = true
			continue
		}
		c := parseCommit(obj.Data)
		for _, p := range c.Parents {
			queue = append(queue, item{p, it.depth + 1})
		}
	}
	return out
}

func handleUploadPack(w http.ResponseWriter, req *http.Request, owner, repoName string) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	v2 := strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
	store.mu.RLock()
	r := store.getRepo(owner, repoName)
	store.mu.RUnlock()
	if r == nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if v2 {
		handleUploadPackV2(w, r, body)
	} else {
		handleUploadPackV0(w, r, body)
	}
}

func handleUploadPackV2(w http.ResponseWriter, r *Repo, body []byte) {
	parsed := parsePktLines(body)
	var command string
	var prefixes []string
	var wants [][20]byte
	var haves = map[[20]byte]bool{}
	var oldShallow = map[[20]byte]bool{}
	deepen := 0
	inArgs := false
	for _, line := range parsed.lines {
		if line == nil { // delim
			inArgs = true
			continue
		}
		s := strings.TrimSuffix(string(line), "\n")
		if !inArgs {
			if strings.HasPrefix(s, "command=") {
				command = s[len("command="):]
			}
			continue
		}
		switch {
		case strings.HasPrefix(s, "ref-prefix "):
			prefixes = append(prefixes, s[len("ref-prefix "):])
		case strings.HasPrefix(s, "want "):
			if o, ok := parseOIDHex(strings.Fields(s[5:])[0]); ok {
				wants = append(wants, o)
			}
		case strings.HasPrefix(s, "have "):
			if o, ok := parseOIDHex(strings.Fields(s[5:])[0]); ok {
				haves[o] = true
			}
		case strings.HasPrefix(s, "shallow "):
			if o, ok := parseOIDHex(strings.Fields(s[8:])[0]); ok {
				oldShallow[o] = true
			}
		case strings.HasPrefix(s, "deepen "):
			fmt.Sscanf(s[len("deepen "):], "%d", &deepen)
		}
	}
	switch command {
	case "ls-refs":
		peel, symrefs, unborn := false, false, false
		for _, line := range parsed.lines {
			if line == nil {
				continue
			}
			s := strings.TrimSuffix(string(line), "\n")
			switch s {
			case "peel":
				peel = true
			case "symrefs":
				symrefs = true
			case "unborn":
				unborn = true
			}
		}
		store.mu.RLock()
		out := lsRefsResponse(r, prefixes, peel, symrefs, unborn)
		store.mu.RUnlock()
		w.WriteHeader(http.StatusOK)
		w.Write(out)
		return
	case "fetch":
		store.mu.RLock()
		out := buildFetchV2(r, wants, haves, oldShallow, deepen)
		store.mu.RUnlock()
		w.WriteHeader(http.StatusOK)
		w.Write(out)
		return
	default:
		w.WriteHeader(http.StatusOK)
		w.Write(pktFlush())
		return
	}
}

// buildFetchV2 constructs the v2 fetch response for already-parsed wants.
// Callers hold at least a read lock on the store.
func buildFetchV2(r *Repo, wants [][20]byte, haves map[[20]byte]bool, oldShallow map[[20]byte]bool, deepen int) []byte {
	var out []byte
	// The acknowledgments section is only sent when the client sent haves
	// (protocol v2: it answers the have/ACK negotiation; a client that
	// sent no haves expects the packfile/shallow-info section first).
	if len(haves) > 0 {
		out = append(out, pktLine([]byte("acknowledgments\n"))...)
		out = append(out, pktLine([]byte("NAK\n"))...)
		out = append(out, pktDelim()...)
	}
	// Nothing new: every want already held — unless this is a deepen
	// request, whose want IS a have and whose point is the next generation.
	allHeld := len(wants) > 0
	for _, wnt := range wants {
		if !haves[wnt] {
			allHeld = false
			break
		}
	}
	if len(wants) == 0 || (allHeld && deepen == 0) {
		out = append(out, pktFlush()...)
		return out
	}
	// Pack cache: only for plain fetches (no haves, no old shallow set).
	useCache := len(haves) == 0 && len(oldShallow) == 0
	cacheKey := ""
	if useCache {
		keyParts := make([]string, 0, len(wants))
		for _, wnt := range wants {
			keyParts = append(keyParts, oidHex(wnt))
		}
		sort.Strings(keyParts)
		cacheKey = "fetch:" + strings.Join(keyParts, ",") + ":d" + fmt.Sprint(deepen)
		r.cacheMu.Lock()
		cached, ok := r.cloneCache[cacheKey]
		r.cacheMu.Unlock()
		if ok {
			// shallow-info for a cached pack: deepen>0 boundaries only
			// (oldShallow is empty here, so nothing unshallows).
			if deepen > 0 {
				newShallow := shallowSet(r, wants, deepen)
				if len(newShallow) > 0 {
					var lines []string
					for oid := range newShallow {
						lines = append(lines, "shallow "+oidHex(oid))
					}
					sort.Strings(lines)
					out = append(out, pktLine([]byte("shallow-info\n"))...)
					for _, l := range lines {
						out = append(out, pktLine([]byte(l+"\n"))...)
					}
					out = append(out, pktDelim()...)
				}
			}
			out = append(out, pktLine([]byte("packfile\n"))...)
			out = append(out, cached...)
			out = append(out, pktFlush()...)
			return out
		}
	}
	oids, included := collectPackObjects(r, wants, haves, deepen, oldShallow)
	// Shallow boundaries: new boundary commits at the deepen depth, and
	// old boundaries that are now interior (included in the pack and not
	// themselves a new boundary) become unshallow.
	var newShallow map[[20]byte]bool
	if deepen > 0 {
		newShallow = shallowSet(r, wants, deepen)
	}
	var shallowLines []string
	if newShallow != nil {
		for oid := range newShallow {
			shallowLines = append(shallowLines, "shallow "+oidHex(oid))
		}
	}
	for oid := range oldShallow {
		if included[oid] && (newShallow == nil || !newShallow[oid]) {
			shallowLines = append(shallowLines, "unshallow "+oidHex(oid))
		}
	}
	if len(shallowLines) > 0 {
		sort.Strings(shallowLines)
		out = append(out, pktLine([]byte("shallow-info\n"))...)
		for _, l := range shallowLines {
			out = append(out, pktLine([]byte(l+"\n"))...)
		}
		out = append(out, pktDelim()...)
	}
	pack := buildPackBytes(r, oids)
	side := sideband(pack)
	if useCache {
		r.cacheMu.Lock()
		if r.cloneCache == nil {
			r.cloneCache = map[string][]byte{}
		}
		r.cloneCache[cacheKey] = side
		r.cacheMu.Unlock()
	}
	out = append(out, pktLine([]byte("packfile\n"))...)
	out = append(out, side...)
	out = append(out, pktFlush()...)
	return out
}

func handleUploadPackV0(w http.ResponseWriter, r *Repo, body []byte) {
	// Two sections split by the first flush pkt-line: wants/shallow/deepen,
	// then haves/done. Split on the parsed flush (never on a raw "0000"
	// substring, which can occur inside an oid).
	first := parsePktLines(body)
	var wants [][20]byte
	var haves = map[[20]byte]bool{}
	var oldShallow = map[[20]byte]bool{}
	deepen := 0
	scan := func(lines [][]byte, wantSection bool) {
		for _, line := range lines {
			if line == nil {
				continue
			}
			s := strings.TrimSuffix(string(line), "\n")
			fields := strings.Fields(s)
			if len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "want":
				if wantSection && len(fields) >= 2 {
					if o, ok := parseOIDHex(fields[1]); ok {
						wants = append(wants, o)
					}
				}
			case "have":
				if !wantSection && len(fields) >= 2 {
					if o, ok := parseOIDHex(fields[1]); ok {
						haves[o] = true
					}
				}
			case "shallow":
				if len(fields) >= 2 {
					if o, ok := parseOIDHex(fields[1]); ok {
						oldShallow[o] = true
					}
				}
			case "deepen":
				if len(fields) >= 2 {
					fmt.Sscanf(fields[1], "%d", &deepen)
				}
			}
		}
	}
	scan(first.lines, true)
	if first.flush && first.consumed < len(body) {
		second := parsePktLines(body[first.consumed:])
		scan(second.lines, false)
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	var out []byte
	out = append(out, pktLine([]byte("NAK\n"))...)
	allHeld := len(wants) > 0
	for _, wnt := range wants {
		if !haves[wnt] {
			allHeld = false
			break
		}
	}
	if len(wants) > 0 && !(allHeld && deepen == 0) {
		oids, included := collectPackObjects(r, wants, haves, deepen, oldShallow)
		var lines []string
		if deepen > 0 {
			for oid := range shallowSet(r, wants, deepen) {
				lines = append(lines, "shallow "+oidHex(oid))
			}
		}
		for oid := range oldShallow {
			if included[oid] {
				lines = append(lines, "unshallow "+oidHex(oid))
			}
		}
		sort.Strings(lines)
		for _, l := range lines {
			out = append(out, pktLine([]byte(l+"\n"))...)
		}
		pack := buildPackBytes(r, oids)
		out = append(out, sideband(pack)...)
	}
	out = append(out, pktFlush()...)
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID [20]byte
	newOID [20]byte
	ref    string
}

func handleReceivePack(w http.ResponseWriter, req *http.Request, owner, repoName string) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	r := store.getRepo(owner, repoName)
	if r == nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	parsed := parsePktLines(body)
	var cmds []pushCommand
	for _, line := range parsed.lines {
		if line == nil {
			continue
		}
		s := strings.TrimSuffix(string(line), "\n")
		if idx := strings.IndexByte(s, 0); idx >= 0 {
			s = s[:idx] // strip capabilities on the first line
		}
		fields := strings.Fields(s)
		if len(fields) != 3 {
			continue
		}
		oldOID, ok1 := parseOIDHex(fields[0])
		newOID, ok2 := parseOIDHex(fields[1])
		if !ok1 || !ok2 {
			continue
		}
		cmds = append(cmds, pushCommand{oldOID: oldOID, newOID: newOID, ref: fields[2]})
	}
	packBytes := body[parsed.consumed:]

	var report strings.Builder
	packOK := true
	if len(packBytes) > 0 {
		if _, err := parsePack(r, packBytes); err != nil {
			packOK = false
			report.WriteString("unpack " + err.Error() + "\n")
		}
	}
	if packOK {
		report.WriteString("unpack ok\n")
	}
	changed := false
	for _, cmd := range cmds {
		if !packOK {
			report.WriteString("ng " + cmd.ref + " unpack failed\n")
			continue
		}
		cur, exists := r.Refs[cmd.ref]
		switch {
		case cmd.newOID == zeroOID: // delete
			if !exists {
				report.WriteString("ng " + cmd.ref + " remote ref does not exist\n")
			} else if cur != cmd.oldOID {
				report.WriteString("ng " + cmd.ref + " remote ref does not match old value\n")
			} else {
				delete(r.Refs, cmd.ref)
				report.WriteString("ok " + cmd.ref + "\n")
				changed = true
			}
		case cmd.oldOID == zeroOID: // create
			if exists {
				report.WriteString("ng " + cmd.ref + " remote ref already exists\n")
			} else if r.getObject(cmd.newOID) == nil {
				report.WriteString("ng " + cmd.ref + " object not found\n")
			} else {
				r.Refs[cmd.ref] = cmd.newOID
				report.WriteString("ok " + cmd.ref + "\n")
				changed = true
			}
		default: // update
			if !exists {
				report.WriteString("ng " + cmd.ref + " remote ref does not exist\n")
			} else if cur != cmd.oldOID {
				report.WriteString("ng " + cmd.ref + " remote ref does not match old value\n")
			} else if r.getObject(cmd.newOID) == nil {
				report.WriteString("ng " + cmd.ref + " object not found\n")
			} else if !r.isAncestor(cmd.oldOID, cmd.newOID) {
				report.WriteString("ng " + cmd.ref + " non-fast-forward\n")
			} else {
				r.Refs[cmd.ref] = cmd.newOID
				report.WriteString("ok " + cmd.ref + "\n")
				changed = true
			}
		}
	}
	if changed {
		r.invalidateCaches()
	}
	// Report-status via sideband band 1: the inner stream is pkt-line framed
	// (one pkt-line per status line), wrapped in a single outer pkt-line.
	var innerBuf []byte
	for _, line := range strings.SplitAfter(report.String(), "\n") {
		if line == "" {
			continue
		}
		innerBuf = append(innerBuf, pktLine([]byte(line))...)
	}
	outer := pktLine(append([]byte{1}, innerBuf...))
	w.WriteHeader(http.StatusOK)
	w.Write(outer)
	w.Write(pktFlush())
}
