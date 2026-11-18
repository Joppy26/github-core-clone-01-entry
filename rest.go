package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

var requestCounter uint64

func nextRequestID() string {
	n := atomic.AddUint64(&requestCounter, 1)
	return fmt.Sprintf("%016x", n)
}

// writeJSONResp writes v with a JSON content type and a request id.
func writeJSONResp(w http.ResponseWriter, status int, v any) {
	b := jsonBytes(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(status)
	w.Write(b)
}

func writeRawJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(status)
	w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSONResp(w, status, map[string]any{"message": msg})
}

// decodeBody decodes a JSON request body into v. An empty body is treated
// as an empty object. Returns false (after writing a 400) on syntax errors.
func decodeBody(w http.ResponseWriter, req *http.Request, v any) bool {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return false
	}
	if len(bytesTrimSpace(body)) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return false
	}
	return true
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// requireAuth enforces a non-empty Bearer token (any value maps to the
// scenario viewer). Returns false after writing a 401 when absent.
func requireAuth(w http.ResponseWriter, req *http.Request) bool {
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(auth[7:]) == "" {
		writeErr(w, http.StatusUnauthorized, "Requires authentication")
		return false
	}
	return true
}

// pagination parses page/per_page (defaults 1/30).
func pagination(req *http.Request) (page, perPage int) {
	page, perPage = 1, 30
	q := req.URL.Query()
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := q.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	return page, perPage
}

// pageSlice slices items for the requested page and reports whether more
// pages follow.
func pageSlice[T any](items []T, page, perPage int) ([]T, bool) {
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}, false
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], end < len(items)
}

// setLinkNext writes a Link header for the next page (absolute URL).
func setLinkNext(w http.ResponseWriter, req *http.Request, page, perPage int) {
	q := req.URL.Query()
	q.Set("page", strconv.Itoa(page+1))
	q.Set("per_page", strconv.Itoa(perPage))
	u := "http://" + req.Host + req.URL.Path + "?" + q.Encode()
	w.Header().Set("Link", "<"+u+">; rel=\"next\"")
}

// ---------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------

func userMap() map[string]any {
	return map[string]any{
		"login": viewerLogin, "id": viewerID, "node_id": "U_" + strconv.Itoa(viewerID),
		"type": "User", "site_admin": false,
	}
}

func repoMap(r *Repo) map[string]any {
	return map[string]any{
		"id": r.ID, "node_id": globalID("R", r.ID), "name": r.Name,
		"full_name": r.fullName(), "private": r.Private,
		"owner":        userMap(),
		"description":  r.Description,
		"default_branch": r.DefaultBranch,
		"created_at":     r.CreatedAt,
		"updated_at":     r.UpdatedAt,
		"html_url":       "http://github.invalid/" + r.fullName(),
		"clone_url":      "http://github.invalid/" + r.fullName() + ".git",
		"git_url":        "git://github.invalid/" + r.fullName() + ".git",
		"ssh_url":        "git@github.invalid:" + r.fullName() + ".git",
		"has_issues": true, "has_projects": true, "has_wiki": true,
		"fork": false, "archived": false, "disabled": false,
		"visibility": map[bool]string{true: "private", false: "public"}[r.Private],
		"license": nil, "topics": []string{},
	}
}

func issueLabels(r *Repo, is *Issue) []any {
	out := []any{}
	for _, name := range is.Labels {
		if l, ok := r.Labels[name]; ok {
			out = append(out, map[string]any{"id": l.ID, "name": l.Name, "color": l.Color})
		} else {
			out = append(out, map[string]any{"name": name, "color": "000000"})
		}
	}
	return out
}

func issueMap(r *Repo, is *Issue) map[string]any {
	var closedAt any
	if is.State == "closed" {
		closedAt = is.ClosedAt
	}
	return map[string]any{
		"id": is.ID(), "node_id": globalID("I", r.ID, int64(is.Number)),
		"number": is.Number, "title": is.Title, "body": is.Body,
		"state": is.State, "labels": issueLabels(r, is),
		"user": userMap(), "assignee": nil, "assignees": []any{},
		"comments":   len(r.Comments[is.Number]),
		"created_at": is.CreatedAt, "updated_at": is.UpdatedAt,
		"closed_at": closedAt,
		"html_url": "http://github.invalid/" + r.fullName() + "/issues/" + strconv.Itoa(is.Number),
		"url": "http://github.invalid/api/v3/repos/" + r.fullName() + "/issues/" + strconv.Itoa(is.Number),
	}
}

// issueID is a stable global REST id for an issue.
func (is *Issue) ID() int64 { return is.Seq }

// cachedIssueJSON returns the pre-serialized GET /issues/{n} response.
func cachedIssueJSON(r *Repo, is *Issue) []byte {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if b, ok := r.issueJSON[is.Number]; ok && r.issueJSONVer[is.Number] == is.Ver {
		return b
	}
	b := jsonBytes(issueMap(r, is))
	if r.issueJSON == nil {
		r.issueJSON = map[int][]byte{}
		r.issueJSONVer = map[int]int64{}
	}
	r.issueJSON[is.Number] = b
	r.issueJSONVer[is.Number] = is.Ver
	return b
}

func prMap(r *Repo, pr *PR) map[string]any {
	var mergedAt, mergeSHA any
	if pr.Merged {
		mergedAt = pr.MergedAt
		mergeSHA = pr.MergeCommit
	}
	mergeable := true
	if pr.State == "open" {
		mergeable = !prHasConflict(r, pr)
	}
	return map[string]any{
		"id": pr.ID(), "node_id": globalID("PR", r.ID, int64(pr.Number)),
		"number": pr.Number, "title": pr.Title, "body": pr.Body,
		"state": pr.State,
		"head": map[string]any{
			"ref": pr.HeadRef, "sha": pr.HeadSHA,
			"label": viewerLogin + ":" + pr.HeadRef, "user": userMap(),
		},
		"base": map[string]any{
			"ref": pr.BaseRef, "sha": pr.BaseSHA,
			"label": viewerLogin + ":" + pr.BaseRef, "user": userMap(),
		},
		"merged": pr.Merged, "merged_at": mergedAt, "merge_commit_sha": mergeSHA,
		"mergeable": mergeable,
		"user":       userMap(),
		"created_at": pr.CreatedAt, "updated_at": pr.UpdatedAt,
		"closed_at": mergedAt,
		"html_url": "http://github.invalid/" + r.fullName() + "/pull/" + strconv.Itoa(pr.Number),
		"url":      "http://github.invalid/api/v3/repos/" + r.fullName() + "/pulls/" + strconv.Itoa(pr.Number),
	}
}

func (pr *PR) ID() int64 { return pr.Seq }

// globalID builds an opaque, stable, decodable global id.
func globalID(kind string, parts ...int64) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		sb.WriteString(":")
		sb.WriteString(strconv.FormatInt(p, 10))
	}
	return b64encode([]byte(sb.String()))
}

// decodeGlobalID parses an id produced by globalID.
func decodeGlobalID(s string) (kind string, parts []int64, ok bool) {
	b, err := b64decode(s)
	if err != nil {
		return "", nil, false
	}
	fields := strings.Split(string(b), ":")
	if len(fields) < 2 {
		return "", nil, false
	}
	kind = fields[0]
	for _, f := range fields[1:] {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return "", nil, false
		}
		parts = append(parts, n)
	}
	return kind, parts, true
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

func handleREST(w http.ResponseWriter, req *http.Request) {
	if !requireAuth(w, req) {
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/api/v3")
	path = strings.Trim(path, "/")
	var segs []string
	if path != "" {
		segs = strings.Split(path, "/")
	}
	routeREST(w, req, segs)
}

func routeREST(w http.ResponseWriter, req *http.Request, segs []string) {
	if len(segs) == 0 {
		// GET /api/v3/
		if req.Method != http.MethodGet {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSONResp(w, http.StatusOK, map[string]any{"message": "forge"})
		return
	}
	switch segs[0] {
	case "meta":
		if req.Method == http.MethodGet {
			writeJSONResp(w, http.StatusOK, map[string]any{"installed_version": "1.0.0"})
			return
		}
	case "user":
		if len(segs) == 1 && req.Method == http.MethodGet {
			writeJSONResp(w, http.StatusOK, userMap())
			return
		}
		if len(segs) == 2 && segs[1] == "repos" {
			switch req.Method {
			case http.MethodGet:
				handleListUserRepos(w, req)
				return
			case http.MethodPost:
				handleCreateRepo(w, req)
				return
			}
		}
	case "repos":
		if len(segs) >= 3 {
			routeRepo(w, req, segs[1], segs[2], segs[3:])
			return
		}
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func getRepoOr404(w http.ResponseWriter, owner, name string) *Repo {
	store.mu.RLock()
	r := store.getRepo(owner, name)
	store.mu.RUnlock()
	if r == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return r
}

func routeRepo(w http.ResponseWriter, req *http.Request, owner, name string, rest []string) {
	if len(rest) == 0 {
		r := getRepoOr404(w, owner, name)
		if r == nil {
			return
		}
		switch req.Method {
		case http.MethodGet:
			store.mu.RLock()
			writeJSONResp(w, http.StatusOK, repoMap(r))
			store.mu.RUnlock()
		case http.MethodPatch:
			var body struct {
				Description *string `json:"description"`
				Private     *bool   `json:"private"`
			}
			if !decodeBody(w, req, &body) {
				return
			}
			store.mu.Lock()
			if body.Description != nil {
				r.Description = *body.Description
			}
			if body.Private != nil {
				r.Private = *body.Private
			}
			r.UpdatedAt = nowStr()
			m := repoMap(r)
			store.mu.Unlock()
			writeJSONResp(w, http.StatusOK, m)
		case http.MethodDelete:
			store.mu.Lock()
			ok := store.deleteRepo(owner, name)
			store.mu.Unlock()
			if !ok {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			w.Header().Set("X-GitHub-Request-Id", nextRequestID())
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "branches":
		if len(rest) == 1 && req.Method == http.MethodGet {
			handleListBranches(w, req, owner, name)
			return
		}
		if len(rest) >= 2 && req.Method == http.MethodGet {
			handleGetBranch(w, req, owner, name, strings.Join(rest[1:], "/"))
			return
		}
	case "commits":
		if req.Method == http.MethodGet {
			handleListCommits(w, req, owner, name)
			return
		}
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			if len(rest) == 2 && req.Method == http.MethodPost {
				handleCreateRef(w, req, owner, name)
				return
			}
			if len(rest) >= 3 && req.Method == http.MethodGet {
				handleGetRef(w, req, owner, name, strings.Join(rest[2:], "/"))
				return
			}
		}
	case "contents":
		if len(rest) >= 1 {
			handleContents(w, req, owner, name, strings.Join(rest[1:], "/"))
			return
		}
	case "labels":
		if req.Method == http.MethodGet {
			handleListLabels(w, req, owner, name)
			return
		}
		if req.Method == http.MethodPost {
			handleCreateLabel(w, req, owner, name)
			return
		}
	case "issues":
		if len(rest) == 1 {
			if req.Method == http.MethodGet {
				handleListIssues(w, req, owner, name)
				return
			}
			if req.Method == http.MethodPost {
				handleCreateIssue(w, req, owner, name)
				return
			}
		}
		if len(rest) >= 2 {
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			if len(rest) == 2 {
				if req.Method == http.MethodGet {
					handleGetIssue(w, req, owner, name, n)
					return
				}
				if req.Method == http.MethodPatch {
					handlePatchIssue(w, req, owner, name, n)
					return
				}
			}
			if len(rest) == 3 && rest[2] == "comments" {
				if req.Method == http.MethodGet {
					handleListComments(w, req, owner, name, n)
					return
				}
				if req.Method == http.MethodPost {
					handleCreateComment(w, req, owner, name, n)
					return
				}
			}
		}
	case "pulls":
		if len(rest) == 1 {
			if req.Method == http.MethodGet {
				handleListPulls(w, req, owner, name)
				return
			}
			if req.Method == http.MethodPost {
				handleCreatePull(w, req, owner, name)
				return
			}
		}
		if len(rest) >= 2 {
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			if len(rest) == 2 && req.Method == http.MethodGet {
				handleGetPull(w, req, owner, name, n)
				return
			}
			if len(rest) == 3 && rest[2] == "files" && req.Method == http.MethodGet {
				handlePullFiles(w, req, owner, name, n)
				return
			}
			if len(rest) == 3 && rest[2] == "merge" && req.Method == http.MethodPut {
				handleMergePull(w, req, owner, name, n)
				return
			}
		}
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// ---------------------------------------------------------------------------
// Repos
// ---------------------------------------------------------------------------

func handleCreateRepo(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	store.mu.Lock()
	r := store.createRepo(viewerLogin, body.Name, body.Description, body.Private)
	if r == nil {
		store.mu.Unlock()
		writeErr(w, http.StatusUnprocessableEntity, "Repository already exists")
		return
	}
	m := repoMap(r)
	store.mu.Unlock()
	writeJSONResp(w, http.StatusCreated, m)
}

func handleListUserRepos(w http.ResponseWriter, req *http.Request) {
	page, perPage := pagination(req)
	store.mu.RLock()
	list := store.byOwner[viewerLogin]
	pageItems, more := pageSlice(list, page, perPage)
	out := []any{}
	for _, r := range pageItems {
		out = append(out, repoMap(r))
	}
	store.mu.RUnlock()
	if more {
		setLinkNext(w, req, page, perPage)
	}
	writeJSONResp(w, http.StatusOK, out)
}

func handleListBranches(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	store.mu.RLock()
	out := []any{}
	for _, ref := range sortedRefNames(r) {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		out = append(out, map[string]any{
			"name": strings.TrimPrefix(ref, "refs/heads/"),
			"commit": map[string]any{"sha": oidHex(r.Refs[ref])},
			"protected": false,
		})
	}
	store.mu.RUnlock()
	writeJSONResp(w, http.StatusOK, out)
}

func handleGetBranch(w http.ResponseWriter, req *http.Request, owner, name, branch string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	store.mu.RLock()
	oid, ok := r.Refs["refs/heads/"+branch]
	store.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSONResp(w, http.StatusOK, map[string]any{
		"name": branch, "commit": map[string]any{"sha": oidHex(oid)}, "protected": false,
	})
}

func handleListCommits(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	shaParam := req.URL.Query().Get("sha")
	store.mu.RLock()
	var tip [20]byte
	var ok bool
	if shaParam == "" {
		tip = r.headOID()
		ok = tip != zeroOID
	} else if oid, perr := parseOIDHex(shaParam); perr {
		tip, ok = oid, r.getObject(oid) != nil
	} else {
		tip, ok = r.Refs["refs/heads/"+shaParam]
		if !ok {
			tip, ok = r.Refs["refs/tags/"+shaParam]
		}
	}
	if !ok {
		store.mu.RUnlock()
		writeJSONResp(w, http.StatusOK, []any{})
		return
	}
	oids := r.walkCommits(tip)
	out := []any{}
	for _, oid := range oids {
		obj := r.getObject(oid)
		if obj == nil {
			continue
		}
		c := parseCommit(obj.Data)
		parents := []any{}
		for _, p := range c.Parents {
			parents = append(parents, map[string]any{"sha": oidHex(p)})
		}
		author := map[string]any{
			"name": commitAuthorName(c.Author), "email": commitAuthorEmail(c.Author),
			"date": commitDate(c.Author),
		}
		out = append(out, map[string]any{
			"sha": oidHex(oid),
			"commit": map[string]any{
				"message": strings.TrimRight(c.Message, "\n"), "author": author, "committer": author,
			},
			"author": userMap(), "committer": userMap(),
			"parents": parents,
		})
	}
	store.mu.RUnlock()
	writeJSONResp(w, http.StatusOK, out)
}

func handleCreateRef(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	var body struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	if body.Ref == "" || body.SHA == "" {
		writeErr(w, http.StatusUnprocessableEntity, "ref and sha are required")
		return
	}
	oid, ok := parseOIDHex(body.SHA)
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "invalid sha")
		return
	}
	store.mu.Lock()
	if _, exists := r.Refs[body.Ref]; exists {
		store.mu.Unlock()
		writeErr(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	if r.getObject(oid) == nil {
		store.mu.Unlock()
		writeErr(w, http.StatusUnprocessableEntity, "object does not exist")
		return
	}
	r.Refs[body.Ref] = oid
	r.invalidateCaches()
	store.mu.Unlock()
	writeJSONResp(w, http.StatusCreated, map[string]any{
		"ref": body.Ref,
		"object": map[string]any{"sha": body.SHA, "type": "commit"},
	})
}

func handleGetRef(w http.ResponseWriter, req *http.Request, owner, name, refPath string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	full := "refs/" + refPath
	store.mu.RLock()
	oid, ok := r.Refs[full]
	store.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSONResp(w, http.StatusOK, map[string]any{
		"ref": full,
		"object": map[string]any{"sha": oidHex(oid), "type": "commit"},
	})
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

// setPathInTree returns a new tree oid with path replaced by blobOID
// (or removed when blobOID is nil), creating intermediate trees as needed.
func setPathInTree(r *Repo, treeOID [20]byte, parts []string, blobOID *[20]byte) [20]byte {
	var entries []TreeEntry
	if treeOID != zeroOID {
		entries = r.listTree(treeOID)
	}
	name := parts[0]
	last := len(parts) == 1
	var out []TreeEntry
	found := false
	for _, e := range entries {
		if e.Name != name {
			out = append(out, e)
			continue
		}
		found = true
		if last {
			if blobOID != nil {
				out = append(out, TreeEntry{Mode: "100644", Name: name, OID: *blobOID})
			}
			// else: drop (delete)
		} else {
			if !e.isDir() {
				// Replace a file with a directory.
				if blobOID != nil {
					sub := setPathInTree(r, zeroOID, parts[1:], blobOID)
					out = append(out, TreeEntry{Mode: "40000", Name: name, OID: sub})
				}
			} else {
				sub := setPathInTree(r, e.OID, parts[1:], blobOID)
				out = append(out, TreeEntry{Mode: "40000", Name: name, OID: sub})
			}
		}
	}
	if !found && blobOID != nil {
		if last {
			out = append(out, TreeEntry{Mode: "100644", Name: name, OID: *blobOID})
		} else {
			sub := setPathInTree(r, zeroOID, parts[1:], blobOID)
			out = append(out, TreeEntry{Mode: "40000", Name: name, OID: sub})
		}
	}
	return r.putObject("tree", buildTree(out))
}

// commitContents creates a commit on branch with the given tree.
func commitContents(r *Repo, branch, message string, treeOID [20]byte) [20]byte {
	var parents [][20]byte
	if tip, ok := r.Refs["refs/heads/"+branch]; ok && tip != zeroOID {
		parents = [][20]byte{tip}
	}
	commitOID := r.putObject("commit", buildCommit(treeOID, parents, message))
	r.Refs["refs/heads/"+branch] = commitOID
	return commitOID
}

func handleContents(w http.ResponseWriter, req *http.Request, owner, name, path string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	switch req.Method {
	case http.MethodGet:
		handleGetContents(w, req, r, path)
	case http.MethodPut:
		handlePutContents(w, req, r, path)
	case http.MethodDelete:
		handleDeleteContents(w, req, r, path)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func resolveContentsRef(r *Repo, refParam string) ([20]byte, bool) {
	if refParam == "" {
		tip := r.headOID()
		return tip, tip != zeroOID
	}
	if oid, ok := parseOIDHex(refParam); ok {
		return oid, r.getObject(oid) != nil
	}
	oid, ok := r.Refs["refs/heads/"+refParam]
	return oid, ok
}

func handleGetContents(w http.ResponseWriter, req *http.Request, r *Repo, path string) {
	refParam := req.URL.Query().Get("ref")
	store.mu.RLock()
	tip, ok := resolveContentsRef(r, refParam)
	if !ok {
		store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	treeOID, ok := r.commitTree(tip)
	if !ok {
		store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	oid, typ, found := r.lookupPath(treeOID, path)
	if !found {
		store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	base := "http://github.invalid/api/v3/repos/" + r.fullName() + "/contents/"
	if typ == "blob" {
		obj := r.getObject(oid)
		writeJSONResp(w, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "size": len(obj.Data),
			"name": lastPathSegment(path), "path": path, "sha": oidHex(oid),
			"content": b64encode(obj.Data),
			"url":         base + path,
			"download_url": "http://github.invalid/" + r.fullName() + "/raw/" + path,
			"html_url":     "http://github.invalid/" + r.fullName() + "/blob/" + path,
		})
		store.mu.RUnlock()
		return
	}
	entries := r.listTree(oid)
	out := []any{}
	for _, e := range entries {
		t := "file"
		size := 0
		if e.isDir() {
			t = "dir"
		} else if obj := r.getObject(e.OID); obj != nil {
			size = len(obj.Data)
		}
		entryPath := e.Name
		if path != "" {
			entryPath = strings.TrimSuffix(path, "/") + "/" + e.Name
		}
		out = append(out, map[string]any{
			"type": t, "size": size, "name": e.Name,
			"path": entryPath,
			"sha": oidHex(e.OID), "url": base + entryPath,
		})
	}
	store.mu.RUnlock()
	writeJSONResp(w, http.StatusOK, out)
}

func lastPathSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func handlePutContents(w http.ResponseWriter, req *http.Request, r *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	data, err := b64decode(body.Content)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "content is not valid base64")
		return
	}
	if body.Message == "" {
		body.Message = "Update " + path
	}
	branch := body.Branch
	if branch == "" {
		branch = r.DefaultBranch
	}
	store.mu.Lock()
	// A named branch that does not exist yet is created from the default tip.
	if _, ok := r.Refs["refs/heads/"+branch]; !ok && branch != r.DefaultBranch {
		r.Refs["refs/heads/"+branch] = r.headOID()
	}
	tip, hasTip := r.Refs["refs/heads/"+branch]
	var treeOID [20]byte
	existed := false
	var curBlob [20]byte
	if hasTip && tip != zeroOID {
		var ok bool
		treeOID, ok = r.commitTree(tip)
		if !ok {
			store.mu.Unlock()
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if oid, typ, found := r.lookupPath(treeOID, path); found && typ == "blob" {
			existed = true
			curBlob = oid
		}
	}
	if existed {
		if body.SHA == "" {
			store.mu.Unlock()
			writeErr(w, http.StatusConflict, "sha is required to update a file")
			return
		}
		if body.SHA != oidHex(curBlob) {
			store.mu.Unlock()
			writeErr(w, http.StatusConflict, "sha does not match the current file")
			return
		}
	}
	blobOID := r.putObject("blob", data)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	newTree := setPathInTree(r, treeOID, parts, &blobOID)
	commitOID := commitContents(r, branch, body.Message, newTree)
	r.invalidateCaches()
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	resp := map[string]any{
		"content": map[string]any{
			"name": lastPathSegment(path), "path": path, "sha": oidHex(blobOID),
			"size": len(data), "encoding": "base64", "content": b64encode(data),
		},
		"commit": map[string]any{"sha": oidHex(commitOID)},
	}
	store.mu.Unlock()
	writeJSONResp(w, status, resp)
}

func handleDeleteContents(w http.ResponseWriter, req *http.Request, r *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = r.DefaultBranch
	}
	store.mu.Lock()
	tip, ok := r.Refs["refs/heads/"+branch]
	if !ok || tip == zeroOID {
		store.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	treeOID, ok := r.commitTree(tip)
	if !ok {
		store.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	oid, typ, found := r.lookupPath(treeOID, path)
	if !found || typ != "blob" {
		store.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.SHA != "" && body.SHA != oidHex(oid) {
		store.mu.Unlock()
		writeErr(w, http.StatusConflict, "sha does not match the current file")
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	newTree := setPathInTree(r, treeOID, parts, nil)
	msg := body.Message
	if msg == "" {
		msg = "Delete " + path
	}
	commitOID := commitContents(r, branch, msg, newTree)
	r.invalidateCaches()
	store.mu.Unlock()
	writeJSONResp(w, http.StatusOK, map[string]any{"commit": map[string]any{"sha": oidHex(commitOID)}})
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

func handleListLabels(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	store.mu.RLock()
	names := make([]string, 0, len(r.Labels))
	for n := range r.Labels {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []any{}
	for _, n := range names {
		l := r.Labels[n]
		out = append(out, map[string]any{"id": l.ID, "name": l.Name, "color": l.Color})
	}
	store.mu.RUnlock()
	writeJSONResp(w, http.StatusOK, out)
}

func handleCreateLabel(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	store.mu.Lock()
	if existing, exists := r.Labels[body.Name]; exists {
		// Idempotent: creating a label that already exists answers 201
		// with the existing label (the suite creates "bug" per repo and
		// treats a 422 here as a setup failure).
		store.mu.Unlock()
		writeJSONResp(w, http.StatusCreated, map[string]any{"id": existing.ID, "name": existing.Name, "color": existing.Color})
		return
	}
	r.LabelSeq++
	l := &Label{ID: r.LabelSeq, Name: body.Name, Color: body.Color}
	if l.Color == "" {
		l.Color = "000000"
	}
	r.Labels[body.Name] = l
	store.mu.Unlock()
	writeJSONResp(w, http.StatusCreated, map[string]any{"id": l.ID, "name": l.Name, "color": l.Color})
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func createIssueLocked(r *Repo, title, body string, labels []string) (*Issue, bool) {
	for _, l := range labels {
		if _, ok := r.Labels[l]; !ok {
			return nil, false
		}
	}
	n := r.NextIssue
	r.NextIssue++
	is := &Issue{
		Number: n, Title: title, Body: body, State: "open",
		Labels: labels, CreatedAt: nowStr(), UpdatedAt: nowStr(),
		Seq: store.nextSeq(), Ver: 1,
	}
	if is.Labels == nil {
		is.Labels = []string{}
	}
	r.Issues = append(r.Issues, is)
	r.IssueByNum[n] = is
	return is, true
}

func handleCreateIssue(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	if body.Title == "" {
		writeErr(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	store.mu.Lock()
	is, ok := createIssueLocked(r, body.Title, body.Body, body.Labels)
	if !ok {
		store.mu.Unlock()
		writeErr(w, http.StatusUnprocessableEntity, "unknown label")
		return
	}
	m := issueMap(r, is)
	store.mu.Unlock()
	writeJSONResp(w, http.StatusCreated, m)
}

func sortedIssues(r *Repo, state, sortDir string) []*Issue {
	var out []*Issue
	for _, is := range r.Issues {
		switch state {
		case "open":
			if is.State != "open" {
				continue
			}
		case "closed":
			if is.State != "closed" {
				continue
			}
		}
		out = append(out, is)
	}
	if sortDir == "desc" {
		sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	} else {
		sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	}
	return out
}

func handleListIssues(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	q := req.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	page, perPage := pagination(req)
	store.mu.RLock()
	list := sortedIssues(r, state, direction)
	pageItems, more := pageSlice(list, page, perPage)
	out := []any{}
	for _, is := range pageItems {
		out = append(out, issueMap(r, is))
	}
	store.mu.RUnlock()
	if more {
		setLinkNext(w, req, page, perPage)
	}
	writeJSONResp(w, http.StatusOK, out)
}

func handleGetIssue(w http.ResponseWriter, req *http.Request, owner, name string, n int) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	store.mu.RLock()
	is := r.IssueByNum[n]
	if is == nil {
		store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	b := cachedIssueJSON(r, is)
	store.mu.RUnlock()
	writeRawJSON(w, http.StatusOK, b)
}

func handlePatchIssue(w http.ResponseWriter, req *http.Request, owner, name string, n int) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	var body struct {
		Title  *string  `json:"title"`
		Body   *string  `json:"body"`
		State  *string  `json:"state"`
		Labels []string `json:"labels"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	store.mu.Lock()
	is := r.IssueByNum[n]
	if is == nil {
		store.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.Title != nil {
		is.Title = *body.Title
	}
	if body.Body != nil {
		is.Body = *body.Body
	}
	if body.State != nil {
		switch *body.State {
		case "open", "closed":
			is.State = *body.State
			if is.State == "closed" {
				is.ClosedAt = nowStr()
			} else {
				is.ClosedAt = ""
			}
		}
	}
	if body.Labels != nil {
		for _, l := range body.Labels {
			if _, ok := r.Labels[l]; !ok {
				store.mu.Unlock()
				writeErr(w, http.StatusUnprocessableEntity, "unknown label")
				return
			}
		}
		is.Labels = body.Labels
	}
	is.UpdatedAt = nowStr()
	is.Ver++
	m := issueMap(r, is)
	store.mu.Unlock()
	writeJSONResp(w, http.StatusOK, m)
}

func handleListComments(w http.ResponseWriter, req *http.Request, owner, name string, n int) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	store.mu.RLock()
	if r.IssueByNum[n] == nil {
		store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	out := []any{}
	for _, c := range r.Comments[n] {
		out = append(out, commentMap(r, n, c))
	}
	store.mu.RUnlock()
	writeJSONResp(w, http.StatusOK, out)
}

func commentMap(r *Repo, issueNum int, c *Comment) map[string]any {
	return map[string]any{
		"id": c.ID, "node_id": globalID("C", r.ID, int64(issueNum), c.ID),
		"body": c.Body, "user": userMap(),
		"created_at": c.CreatedAt, "updated_at": c.CreatedAt,
	}
}

func createCommentLocked(r *Repo, issueNum int, body string) *Comment {
	r.CommentSeq++
	c := &Comment{ID: r.CommentSeq, Body: body, CreatedAt: nowStr()}
	r.Comments[issueNum] = append(r.Comments[issueNum], c)
	return c
}

func handleCreateComment(w http.ResponseWriter, req *http.Request, owner, name string, n int) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	store.mu.Lock()
	if r.IssueByNum[n] == nil {
		store.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	c := createCommentLocked(r, n, body.Body)
	m := commentMap(r, n, c)
	store.mu.Unlock()
	writeJSONResp(w, http.StatusCreated, m)
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

func createPullLocked(r *Repo, title, body, head, base string) (*PR, int) {
	headOID, ok := r.Refs["refs/heads/"+head]
	if !ok {
		return nil, http.StatusUnprocessableEntity
	}
	baseOID, ok := r.Refs["refs/heads/"+base]
	if !ok {
		return nil, http.StatusUnprocessableEntity
	}
	for _, pr := range r.PRs {
		if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
			return nil, http.StatusUnprocessableEntity
		}
	}
	n := r.NextPR
	r.NextPR++
	pr := &PR{
		Number: n, Title: title, Body: body, State: "open",
		HeadRef: head, HeadSHA: oidHex(headOID),
		BaseRef: base, BaseSHA: oidHex(baseOID),
		CreatedAt: nowStr(), UpdatedAt: nowStr(),
		Seq: store.nextSeq(),
	}
	r.PRs = append(r.PRs, pr)
	r.PRByNum[n] = pr
	return pr, 0
}

func handleCreatePull(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	if body.Title == "" || body.Head == "" || body.Base == "" {
		writeErr(w, http.StatusUnprocessableEntity, "title, head and base are required")
		return
	}
	store.mu.Lock()
	pr, code := createPullLocked(r, body.Title, body.Body, body.Head, body.Base)
	if pr == nil {
		store.mu.Unlock()
		writeErr(w, code, "cannot create pull request")
		return
	}
	m := prMap(r, pr)
	store.mu.Unlock()
	writeJSONResp(w, http.StatusCreated, m)
}

func handleListPulls(w http.ResponseWriter, req *http.Request, owner, name string) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	state := req.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	page, perPage := pagination(req)
	store.mu.RLock()
	var list []*PR
	for _, pr := range r.PRs {
		switch state {
		case "open":
			if pr.State != "open" {
				continue
			}
		case "closed":
			if pr.State != "closed" {
				continue
			}
		}
		list = append(list, pr)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
	pageItems, more := pageSlice(list, page, perPage)
	out := []any{}
	for _, pr := range pageItems {
		out = append(out, prMap(r, pr))
	}
	store.mu.RUnlock()
	if more {
		setLinkNext(w, req, page, perPage)
	}
	writeJSONResp(w, http.StatusOK, out)
}

func handleGetPull(w http.ResponseWriter, req *http.Request, owner, name string, n int) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	store.mu.RLock()
	pr := r.PRByNum[n]
	if pr == nil {
		store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	m := prMap(r, pr)
	store.mu.RUnlock()
	writeJSONResp(w, http.StatusOK, m)
}

// fileChange is one entry of a tree diff.
type fileChange struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
}

// flattenTree returns path -> (oid, mode) for every blob in a tree.
func flattenTree(r *Repo, treeOID [20]byte, prefix string, out map[string][20]byte) {
	for _, e := range r.listTree(treeOID) {
		p := prefix + e.Name
		if e.isDir() {
			flattenTree(r, e.OID, p+"/", out)
		} else {
			out[p] = e.OID
		}
	}
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff counts added/removed lines between two blobs (multiset difference).
func lineDiff(oldData, newData []byte) (add, del int) {
	oldLines := map[string]int{}
	for _, l := range strings.Split(string(oldData), "\n") {
		oldLines[l]++
	}
	newLines := map[string]int{}
	for _, l := range strings.Split(string(newData), "\n") {
		newLines[l]++
	}
	for l, c := range newLines {
		if oldLines[l] < c {
			add += c - oldLines[l]
		}
	}
	for l, c := range oldLines {
		if newLines[l] < c {
			del += c - newLines[l]
		}
	}
	// Trailing empty string from Split is not a line.
	return add, del
}

// diffTrees compares two trees, returning changed files sorted by path.
func diffTrees(r *Repo, baseTree, headTree [20]byte) []fileChange {
	baseFiles := map[string][20]byte{}
	headFiles := map[string][20]byte{}
	if baseTree != zeroOID {
		flattenTree(r, baseTree, "", baseFiles)
	}
	if headTree != zeroOID {
		flattenTree(r, headTree, "", headFiles)
	}
	var paths []string
	seen := map[string]bool{}
	for p := range baseFiles {
		paths = append(paths, p)
		seen[p] = true
	}
	for p := range headFiles {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var out []fileChange
	for _, p := range paths {
		b, inBase := baseFiles[p]
		h, inHead := headFiles[p]
		switch {
		case inBase && !inHead:
			del := 0
			if obj := r.getObject(b); obj != nil {
				del = countLines(obj.Data)
			}
			out = append(out, fileChange{Path: p, Status: "removed", Additions: 0, Deletions: del})
		case !inBase && inHead:
			add := 0
			if obj := r.getObject(h); obj != nil {
				add = countLines(obj.Data)
			}
			out = append(out, fileChange{Path: p, Status: "added", Additions: add, Deletions: 0})
		case b != h:
			var oldData, newData []byte
			if obj := r.getObject(b); obj != nil {
				oldData = obj.Data
			}
			if obj := r.getObject(h); obj != nil {
				newData = obj.Data
			}
			add, del := lineDiff(oldData, newData)
			if add == 0 && del == 0 {
				add = 1 // content changed but line counts equal (e.g. 1-line swap)
			}
			out = append(out, fileChange{Path: p, Status: "modified", Additions: add, Deletions: del})
		}
	}
	return out
}

// prTrees returns (baseTree, headTree) for a PR.
func prTrees(r *Repo, pr *PR) ([20]byte, [20]byte, bool) {
	baseOID, ok := parseOIDHex(pr.BaseSHA)
	if !ok {
		return zeroOID, zeroOID, false
	}
	headOID, ok := parseOIDHex(pr.HeadSHA)
	if !ok {
		return zeroOID, zeroOID, false
	}
	baseTree, ok1 := r.commitTree(baseOID)
	headTree, ok2 := r.commitTree(headOID)
	return baseTree, headTree, ok1 && ok2
}

// prHasConflict reports whether a file changed by the PR was also changed
// at the current base tip relative to the PR's base.
func prHasConflict(r *Repo, pr *PR) bool {
	baseTree, headTree, ok := prTrees(r, pr)
	if !ok {
		return false
	}
	changes := diffTrees(r, baseTree, headTree)
	if len(changes) == 0 {
		return false
	}
	curBaseOID, ok := r.Refs["refs/heads/"+pr.BaseRef]
	if !ok {
		return false
	}
	curBaseTree, ok := r.commitTree(curBaseOID)
	if !ok {
		return false
	}
	baseFiles := map[string][20]byte{}
	curFiles := map[string][20]byte{}
	flattenTree(r, baseTree, "", baseFiles)
	flattenTree(r, curBaseTree, "", curFiles)
	for _, ch := range changes {
		b, inBase := baseFiles[ch.Path]
		c, inCur := curFiles[ch.Path]
		if inBase != inCur || (inBase && b != c) {
			return true
		}
	}
	return false
}

func handlePullFiles(w http.ResponseWriter, req *http.Request, owner, name string, n int) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	store.mu.RLock()
	pr := r.PRByNum[n]
	if pr == nil {
		store.mu.RUnlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	baseTree, headTree, ok := prTrees(r, pr)
	if !ok {
		store.mu.RUnlock()
		writeJSONResp(w, http.StatusOK, []any{})
		return
	}
	changes := diffTrees(r, baseTree, headTree)
	out := []any{}
	for _, ch := range changes {
		out = append(out, map[string]any{
			"filename": ch.Path, "status": ch.Status,
			"additions": ch.Additions, "deletions": ch.Deletions,
			"changes": ch.Additions + ch.Deletions,
		})
	}
	store.mu.RUnlock()
	writeJSONResp(w, http.StatusOK, out)
}

func handleMergePull(w http.ResponseWriter, req *http.Request, owner, name string, n int) {
	r := getRepoOr404(w, owner, name)
	if r == nil {
		return
	}
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	if !decodeBody(w, req, &body) {
		return
	}
	store.mu.Lock()
	pr := r.PRByNum[n]
	if pr == nil {
		store.mu.Unlock()
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if pr.State != "open" {
		store.mu.Unlock()
		writeErr(w, http.StatusMethodNotAllowed, "pull request is not open")
		return
	}
	if prHasConflict(r, pr) {
		store.mu.Unlock()
		writeErr(w, http.StatusMethodNotAllowed, "merge conflict")
		return
	}
	sha, ok := mergePullLocked(r, pr)
	if !ok {
		store.mu.Unlock()
		writeErr(w, http.StatusMethodNotAllowed, "cannot merge")
		return
	}
	store.mu.Unlock()
	writeJSONResp(w, http.StatusOK, map[string]any{
		"merged": true, "sha": sha, "message": "Merged pull request #" + strconv.Itoa(n),
	})
}

// mergePullLocked squash-merges an open, non-conflicted PR. Caller holds the
// write lock. Returns the merge commit sha.
func mergePullLocked(r *Repo, pr *PR) (string, bool) {
	headOID, ok := parseOIDHex(pr.HeadSHA)
	if !ok {
		return "", false
	}
	headTree, ok := r.commitTree(headOID)
	if !ok {
		return "", false
	}
	baseOID, ok := r.Refs["refs/heads/"+pr.BaseRef]
	if !ok || baseOID == zeroOID {
		return "", false
	}
	commitOID := r.putObject("commit", buildCommit(headTree, [][20]byte{baseOID}, pr.Title))
	r.Refs["refs/heads/"+pr.BaseRef] = commitOID
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = nowStr()
	pr.MergeCommit = oidHex(commitOID)
	pr.UpdatedAt = nowStr()
	r.invalidateCaches()
	return oidHex(commitOID), true
}
