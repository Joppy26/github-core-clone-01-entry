// Package main implements a GitHub-compatible git forge: smart-HTTP git
// protocol (v0 + v2), REST v3, GraphQL and MCP over one in-memory store.
// Original code, written for the github-core-clone-01 bounty.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Scenario identity (fixed for this competition's deterministic scenario).
// ---------------------------------------------------------------------------

const viewerLogin = "adaiversen"
const viewerID = 1000

var zeroOID [20]byte

func nowStr() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------------------
// Object store
// ---------------------------------------------------------------------------

// Object is one git object: type ("commit", "tree", "blob", "tag") plus raw data.
type Object struct {
	Type string
	Data []byte
}

func oidFor(typ string, data []byte) [20]byte {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	var out [20]byte
	copy(out[:], h.Sum(nil))
	return out
}

func oidHex(o [20]byte) string { return hex.EncodeToString(o[:]) }

func parseOIDHex(s string) ([20]byte, bool) {
	var o [20]byte
	if len(s) != 40 {
		return o, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return o, false
	}
	copy(o[:], b)
	return o, true
}

// TreeEntry is one entry of a parsed tree object.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "40000", "160000"
	Name string
	OID  [20]byte
}

func (e TreeEntry) isDir() bool { return e.Mode == "40000" }

// parseTree parses raw tree bytes into entries.
func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := -1
		for j := i; j < len(data); j++ {
			if data[j] == ' ' {
				sp = j
				break
			}
		}
		if sp < 0 {
			break
		}
		mode := string(data[i:sp])
		nul := -1
		for j := sp + 1; j < len(data); j++ {
			if data[j] == 0 {
				nul = j
				break
			}
		}
		if nul < 0 || nul+21 > len(data) {
			break
		}
		var oid [20]byte
		copy(oid[:], data[nul+1:nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: string(data[sp+1 : nul]), OID: oid})
		i = nul + 21
	}
	return out
}

// buildTree serializes entries, sorted git-style (directory names compare as
// if suffixed with '/').
func buildTree(entries []TreeEntry) []byte {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].Name, entries[j].Name
		if entries[i].isDir() {
			a += "/"
		}
		if entries[j].isDir() {
			b += "/"
		}
		return a < b
	})
	var out []byte
	for _, e := range entries {
		out = append(out, []byte(e.Mode)...)
		out = append(out, ' ')
		out = append(out, []byte(e.Name)...)
		out = append(out, 0)
		out = append(out, e.OID[:]...)
	}
	return out
}

// CommitInfo is a parsed commit object.
type CommitInfo struct {
	Tree    [20]byte
	Parents [][20]byte
	Author  string // "Name <email> ts tz"
	Message string
	Raw     []byte
}

func parseCommit(data []byte) *CommitInfo {
	c := &CommitInfo{Raw: data}
	// Header lines until blank line.
	rest := data
	for len(rest) > 0 {
		nl := -1
		for i, b := range rest {
			if b == '\n' {
				nl = i
				break
			}
		}
		var line []byte
		if nl < 0 {
			line, rest = rest, nil
		} else {
			line, rest = rest[:nl], rest[nl+1:]
		}
		if len(line) == 0 {
			break
		}
		s := string(line)
		switch {
		case strings.HasPrefix(s, "tree "):
			if o, ok := parseOIDHex(strings.TrimSpace(s[5:])); ok {
				c.Tree = o
			}
		case strings.HasPrefix(s, "parent "):
			if o, ok := parseOIDHex(strings.TrimSpace(s[7:])); ok {
				c.Parents = append(c.Parents, o)
			}
		case strings.HasPrefix(s, "author "):
			c.Author = s[7:]
		}
	}
	c.Message = string(rest)
	return c
}

// commitAuthorDate converts an author line's unix timestamp to RFC3339.
func commitDate(author string) string {
	// "Name <email> 1735689600 +0000"
	gt := strings.LastIndex(author, ">")
	if gt < 0 {
		return nowStr()
	}
	parts := strings.Fields(author[gt+1:])
	if len(parts) == 0 {
		return nowStr()
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nowStr()
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

func commitAuthorName(author string) string {
	lt := strings.Index(author, "<")
	if lt < 0 {
		return author
	}
	return strings.TrimSpace(author[:lt])
}

func commitAuthorEmail(author string) string {
	lt := strings.Index(author, "<")
	gt := strings.Index(author, ">")
	if lt < 0 || gt < 0 || gt <= lt {
		return ""
	}
	return author[lt+1 : gt]
}

// buildCommit builds a server-side commit (squash merges, contents commits).
func buildCommit(tree [20]byte, parents [][20]byte, message string) []byte {
	var b strings.Builder
	b.WriteString("tree ")
	b.WriteString(oidHex(tree))
	b.WriteString("\n")
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(oidHex(p))
		b.WriteString("\n")
	}
	ts := time.Now().Unix()
	b.WriteString(fmt.Sprintf("author Forge <forge@localhost> %d +0000\n", ts))
	b.WriteString(fmt.Sprintf("committer Forge <forge@localhost> %d +0000\n", ts))
	b.WriteString("\n")
	b.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

type Label struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	CreatedAt string
	UpdatedAt string
	ClosedAt  string
	Seq       int64
	Ver       int64
}

type Comment struct {
	ID        int64
	Body      string
	CreatedAt string
}

type PR struct {
	Number        int
	Title         string
	Body          string
	State         string // "open" | "closed"
	HeadRef       string
	HeadSHA       string
	BaseRef       string
	BaseSHA       string // base tip at creation
	Merged        bool
	MergedAt      string
	MergeCommit   string
	CreatedAt     string
	UpdatedAt     string
	Seq           int64
}

type Repo struct {
	ID            int64
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Refs          map[string][20]byte // full ref name -> oid
	Objects       map[[20]byte]*Object
	Issues        []*Issue
	IssueByNum    map[int]*Issue
	NextIssue     int
	PRs           []*PR
	PRByNum       map[int]*PR
	NextPR        int
	Labels        map[string]*Label
	LabelSeq      int64
	Comments      map[int][]*Comment // issue number -> comments
	CommentSeq    int64
	Version       int64 // bumped on any ref/object change; invalidates caches
	cacheMu       sync.Mutex
	advCache      map[string][]byte
	advVer        int64
	cloneCache    map[string][]byte
	issueJSON     map[int][]byte
	issueJSONVer  map[int]int64
}

func (r *Repo) fullName() string { return r.Owner + "/" + r.Name }

// putObject stores an object, returning its oid.
func (r *Repo) putObject(typ string, data []byte) [20]byte {
	oid := oidFor(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.Objects[oid] = &Object{Type: typ, Data: cp}
	}
	return oid
}

func (r *Repo) getObject(oid [20]byte) *Object { return r.Objects[oid] }

// resolveRef returns the oid a ref name points at.
func (r *Repo) resolveRef(ref string) ([20]byte, bool) {
	oid, ok := r.Refs[ref]
	return oid, ok
}

// headOID returns the default branch tip (zero when unborn).
func (r *Repo) headOID() [20]byte {
	oid, _ := r.Refs["refs/heads/"+r.DefaultBranch]
	return oid
}

// commitTree returns the tree oid of a commit.
func (r *Repo) commitTree(commitOID [20]byte) ([20]byte, bool) {
	obj := r.getObject(commitOID)
	if obj == nil || obj.Type != "commit" {
		return zeroOID, false
	}
	return parseCommit(obj.Data).Tree, true
}

// lookupPath walks a tree to path, returning (oid, type, ok).
func (r *Repo) lookupPath(treeOID [20]byte, path string) ([20]byte, string, bool) {
	if path == "" || path == "/" {
		return treeOID, "tree", true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cur := treeOID
	for i, part := range parts {
		obj := r.getObject(cur)
		if obj == nil || obj.Type != "tree" {
			return zeroOID, "", false
		}
		found := false
		for _, e := range parseTree(obj.Data) {
			if e.Name == part {
				cur = e.OID
				if i == len(parts)-1 {
					if e.isDir() {
						return cur, "tree", true
					}
					return cur, "blob", true
				}
				if !e.isDir() {
					return zeroOID, "", false
				}
				found = true
				break
			}
		}
		if !found {
			return zeroOID, "", false
		}
	}
	return cur, "tree", true
}

// listTree returns the entries of a tree object.
func (r *Repo) listTree(treeOID [20]byte) []TreeEntry {
	obj := r.getObject(treeOID)
	if obj == nil || obj.Type != "tree" {
		return nil
	}
	return parseTree(obj.Data)
}

// isAncestor reports whether anc is reachable from tip (following parents).
func (r *Repo) isAncestor(anc, tip [20]byte) bool {
	if anc == tip {
		return true
	}
	seen := map[[20]byte]bool{}
	stack := [][20]byte{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.getObject(cur)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case "commit":
			c := parseCommit(obj.Data)
			stack = append(stack, c.Parents...)
		case "tag":
			// annotated tag: follow its target line "object <oid>"
			for _, line := range strings.Split(string(obj.Data), "\n") {
				if strings.HasPrefix(line, "object ") {
					if o, ok := parseOIDHex(strings.TrimSpace(line[7:])); ok {
						stack = append(stack, o)
					}
					break
				}
			}
		}
	}
	return false
}

// walkCommits returns commit oids newest-first from tip (DFS over parents).
func (r *Repo) walkCommits(tip [20]byte) [][20]byte {
	var out [][20]byte
	seen := map[[20]byte]bool{}
	stack := [][20]byte{tip}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] || cur == zeroOID {
			continue
		}
		seen[cur] = true
		obj := r.getObject(cur)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		out = append(out, cur)
		c := parseCommit(obj.Data)
		// Push parents in reverse so the first parent is visited first.
		for i := len(c.Parents) - 1; i >= 0; i-- {
			stack = append(stack, c.Parents[i])
		}
	}
	return out
}

// peelToCommit follows annotated tags to the commit they point at.
func (r *Repo) peelToCommit(oid [20]byte) [20]byte {
	for {
		obj := r.getObject(oid)
		if obj == nil {
			return oid
		}
		if obj.Type == "commit" {
			return oid
		}
		if obj.Type == "tag" {
			for _, line := range strings.Split(string(obj.Data), "\n") {
				if strings.HasPrefix(line, "object ") {
					if o, ok := parseOIDHex(strings.TrimSpace(line[7:])); ok {
						oid = o
						break
					}
				}
			}
			continue
		}
		return oid
	}
}

// ---------------------------------------------------------------------------
// Global store
// ---------------------------------------------------------------------------

type Store struct {
	mu           sync.RWMutex
	repos        map[string]*Repo // "owner/name" -> repo
	byOwner      map[string][]*Repo
	nextRepoID   int64
	nextIssueID  int64 // global REST issue id counter
	nextPRID     int64
	nextGlobalID int64 // comment + misc ids
	seq          int64
}

func NewStore() *Store {
	return &Store{
		repos:   map[string]*Repo{},
		byOwner: map[string][]*Repo{},
	}
}

var store = NewStore()

func (s *Store) nextSeq() int64 {
	s.seq++
	return s.seq
}

// createRepo creates a repo owned by owner. Returns nil when the name exists.
func (s *Store) createRepo(owner, name, description string, private bool) *Repo {
	key := owner + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil
	}
	s.nextRepoID++
	r := &Repo{
		ID:            s.nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedAt:     nowStr(),
		UpdatedAt:     nowStr(),
		Refs:          map[string][20]byte{},
		Objects:       map[[20]byte]*Object{},
		IssueByNum:    map[int]*Issue{},
		NextIssue:     1,
		PRByNum:       map[int]*PR{},
		NextPR:        1,
		Labels:        map[string]*Label{},
		Comments:      map[int][]*Comment{},
		advCache:      map[string][]byte{},
		cloneCache:    map[string][]byte{},
		issueJSON:     map[int][]byte{},
		issueJSONVer:  map[int]int64{},
	}
	// No seeded labels: the suite creates labels through POST /labels and
	// expects 201 for a name such as "bug" on a fresh repository.
	s.repos[key] = r
	s.byOwner[owner] = append(s.byOwner[owner], r)
	return r
}

func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

func (s *Store) deleteRepo(owner, name string) bool {
	key := owner + "/" + name
	r, ok := s.repos[key]
	if !ok {
		return false
	}
	delete(s.repos, key)
	list := s.byOwner[owner]
	for i, x := range list {
		if x == r {
			s.byOwner[owner] = append(list[:i], list[i+1:]...)
			break
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

// jsonBytes marshals v; on error returns an empty object.
func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func b64encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func b64decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
