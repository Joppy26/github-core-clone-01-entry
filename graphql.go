package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type gqlToken struct {
	kind string // "name", "string", "int", "float", "punct", "var", "eof"
	val  string
}

func lexGraphQL(src string) ([]gqlToken, error) {
	var toks []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '"':
			// string (with escapes); block strings """ are not used by the suite
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated string")
				}
				toks = append(toks, gqlToken{"string", src[i+3 : i+3+end]})
				i += 3 + end + 3
				continue
			}
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(src[j])
					}
					j++
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlToken{"string", sb.String()})
			i = j + 1
		case c == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			toks = append(toks, gqlToken{"var", src[i+1 : j]})
			i = j
		case c == '.' && strings.HasPrefix(src[i:], "..."):
			toks = append(toks, gqlToken{"punct", "..."})
			i += 3
		case strings.ContainsRune("!$():=@[]{ }|", rune(c)):
			toks = append(toks, gqlToken{"punct", string(c)})
			i++
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlToken{kind, src[i:j]})
			i = j
		case isNameStart(c):
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			toks = append(toks, gqlToken{"name", src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gqlToken{"eof", ""})
	return toks, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type gqlField struct {
	alias    string
	name     string
	args     map[string]any
	children []*gqlField
	typeCond string // for inline fragments: "... on Type"
}

type gqlOperation struct {
	kind      string // "query" | "mutation"
	name      string
	variables map[string]any // resolved values (from the request variables)
	sel       []*gqlField
}

type gqlParser struct {
	toks []gqlToken
	pos  int
	vars map[string]any
}

func (p *gqlParser) peek() gqlToken  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlToken  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind, val string) (gqlToken, error) {
	t := p.next()
	if t.kind != kind || (val != "" && t.val != val) {
		return t, fmt.Errorf("expected %s %q, got %s %q", kind, val, t.kind, t.val)
	}
	return t, nil
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, error) {
	var ops []*gqlOperation
	for p.peek().kind != "eof" {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query", variables: p.vars}
	t := p.peek()
	if t.kind == "name" && (t.val == "query" || t.val == "mutation") {
		op.kind = p.next().val
		if p.peek().kind == "name" {
			op.name = p.next().val
		}
	} else if t.kind == "punct" && t.val == "{" {
		// shorthand query
	} else {
		return nil, fmt.Errorf("expected operation")
	}
	// variable definitions: ($x: Type = default, ...)
	if p.peek().kind == "punct" && p.peek().val == "(" {
		if err := p.parseVarDefs(); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

func (p *gqlParser) parseVarDefs() error {
	if _, err := p.expect("punct", "("); err != nil {
		return err
	}
	for {
		t := p.next()
		if t.kind == "punct" && t.val == ")" {
			return nil
		}
		if t.kind != "var" {
			return fmt.Errorf("expected variable in var defs")
		}
		// : Type (possibly [T!]! etc.)
		if _, err := p.expect("punct", ":"); err != nil {
			return err
		}
		if err := p.parseTypeRef(); err != nil {
			return err
		}
		// optional = default
		if p.peek().kind == "punct" && p.peek().val == "=" {
			p.next()
			if _, err := p.parseValue(); err != nil {
				return err
			}
		}
	}
}

// parseTypeRef consumes a type reference (Name, [T], T!, etc.).
func (p *gqlParser) parseTypeRef() error {
	t := p.next()
	if t.kind == "punct" && t.val == "[" {
		if err := p.parseTypeRef(); err != nil {
			return err
		}
		if _, err := p.expect("punct", "]"); err != nil {
			return err
		}
	} else if t.kind != "name" {
		return fmt.Errorf("expected type name")
	}
	if p.peek().kind == "punct" && p.peek().val == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if _, err := p.expect("punct", "{"); err != nil {
		return nil, err
	}
	var fields []*gqlField
	for {
		t := p.peek()
		if t.kind == "punct" && t.val == "}" {
			p.next()
			return fields, nil
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) parseField() (*gqlField, error) {
	t := p.peek()
	if t.kind == "punct" && t.val == "..." {
		p.next()
		f := &gqlField{}
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			nameTok := p.next()
			if nameTok.kind != "name" {
				return nil, fmt.Errorf("expected type name after ... on")
			}
			f.typeCond = nameTok.val
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.children = sel
		return f, nil
	}
	nameTok := p.next()
	if nameTok.kind != "name" {
		return nil, fmt.Errorf("expected field name, got %s %q", nameTok.kind, nameTok.val)
	}
	f := &gqlField{name: nameTok.val, alias: nameTok.val}
	if p.peek().kind == "punct" && p.peek().val == ":" {
		p.next()
		aliasTok := p.next()
		if aliasTok.kind != "name" {
			return nil, fmt.Errorf("expected field name after alias colon")
		}
		f.alias = f.name
		f.name = aliasTok.val
	}
	if p.peek().kind == "punct" && p.peek().val == "(" {
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	if p.peek().kind == "punct" && p.peek().val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.children = sel
	}
	return f, nil
}

func (p *gqlParser) parseArgs() (map[string]any, error) {
	if _, err := p.expect("punct", "("); err != nil {
		return nil, err
	}
	args := map[string]any{}
	for {
		t := p.next()
		if t.kind == "punct" && t.val == ")" {
			return args, nil
		}
		if t.kind != "name" {
			return nil, fmt.Errorf("expected argument name")
		}
		if _, err := p.expect("punct", ":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[t.val] = v
	}
}

// parseValue parses a GraphQL value literal (or variable reference).
func (p *gqlParser) parseValue() (any, error) {
	t := p.next()
	switch t.kind {
	case "var":
		if p.vars == nil {
			return nil, nil
		}
		v, ok := p.vars[t.val]
		if !ok {
			return nil, nil
		}
		return v, nil
	case "string":
		return t.val, nil
	case "int":
		n, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "float":
		f, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, err
		}
		return f, nil
	case "name":
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return t.val, nil // enum value: keep as string
		}
	case "punct":
		switch t.val {
		case "[":
			var list []any
			for {
				if p.peek().kind == "punct" && p.peek().val == "]" {
					p.next()
					return list, nil
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
		case "{":
			obj := map[string]any{}
			for {
				if p.peek().kind == "punct" && p.peek().val == "}" {
					p.next()
					return obj, nil
				}
				nameTok := p.next()
				if nameTok.kind != "name" {
					return nil, fmt.Errorf("expected object field name")
				}
				if _, err := p.expect("punct", ":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[nameTok.val] = v
			}
		}
	}
	return nil, fmt.Errorf("unexpected value token %s %q", t.kind, t.val)
}

// ---------------------------------------------------------------------------
// Introspection schema (static)
// ---------------------------------------------------------------------------

// gqlFieldDef describes one schema field: name + type reference string
// ("ID!", "String", "[Issue!]!", "IssueState", ...).
type gqlFieldDef struct {
	name string
	typ  string
}

type gqlTypeDef struct {
	kind       string // OBJECT, ENUM, SCALAR
	fields     []gqlFieldDef
	enumValues []string
}

var gqlSchema = map[string]*gqlTypeDef{
	"Query": {kind: "OBJECT", fields: []gqlFieldDef{
		{"viewer", "User"},
		{"repository", "Repository"},
		{"node", "Node"},
		{"__type", "__Type"},
		{"__schema", "__Schema"},
	}},
	"Mutation": {kind: "OBJECT", fields: []gqlFieldDef{
		{"createIssue", "CreateIssuePayload"},
		{"closeIssue", "CloseIssuePayload"},
		{"createPullRequest", "CreatePullRequestPayload"},
		{"mergePullRequest", "MergePullRequestPayload"},
		{"addComment", "AddCommentPayload"},
	}},
	"Repository": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"databaseId", "Int"},
		{"name", "String!"},
		{"nameWithOwner", "String!"},
		{"owner", "User!"},
		{"description", "String"},
		{"defaultBranchRef", "Ref"},
		{"viewerPermission", "RepositoryPermission"},
		{"hasIssuesEnabled", "Boolean!"},
		{"url", "String!"},
		{"createdAt", "String"},
		{"updatedAt", "String"},
		{"issue", "Issue"},
		{"issues", "IssueConnection!"},
		{"pullRequest", "PullRequest"},
		{"pullRequests", "PullRequestConnection!"},
		{"labels", "LabelConnection"},
	}},
	"Issue": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"databaseId", "Int"},
		{"number", "Int!"},
		{"title", "String!"},
		{"body", "String"},
		{"state", "IssueState!"},
		{"author", "User"},
		{"createdAt", "String!"},
		{"updatedAt", "String!"},
		{"closedAt", "String"},
		{"url", "String!"},
		{"repository", "Repository!"},
		{"comments", "IssueCommentConnection!"},
		{"labels", "LabelConnection"},
	}},
	"PullRequest": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"databaseId", "Int"},
		{"number", "Int!"},
		{"title", "String!"},
		{"body", "String"},
		{"state", "PullRequestState!"},
		{"merged", "Boolean!"},
		{"mergeable", "MergeableState!"},
		{"headRefName", "String!"},
		{"baseRefName", "String!"},
		{"headRef", "Ref"},
		{"baseRef", "Ref"},
		{"files", "PullRequestChangedFileConnection"},
		{"author", "User"},
		{"createdAt", "String!"},
		{"updatedAt", "String!"},
		{"url", "String!"},
		{"repository", "Repository!"},
	}},
	"User": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"login", "String!"},
		{"name", "String"},
		{"url", "String!"},
	}},
	"Ref": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"name", "String!"},
		{"prefix", "String!"},
		{"target", "GitObject"},
	}},
	"GitObject": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"oid", "String!"},
	}},
	"IssueConnection": {kind: "OBJECT", fields: []gqlFieldDef{
		{"nodes", "[Issue]"},
		{"edges", "[IssueEdge]"},
		{"pageInfo", "PageInfo!"},
		{"totalCount", "Int!"},
	}},
	"IssueEdge": {kind: "OBJECT", fields: []gqlFieldDef{
		{"node", "Issue"},
		{"cursor", "String!"},
	}},
	"PullRequestConnection": {kind: "OBJECT", fields: []gqlFieldDef{
		{"nodes", "[PullRequest]"},
		{"edges", "[PullRequestEdge]"},
		{"pageInfo", "PageInfo!"},
		{"totalCount", "Int!"},
	}},
	"PullRequestEdge": {kind: "OBJECT", fields: []gqlFieldDef{
		{"node", "PullRequest"},
		{"cursor", "String!"},
	}},
	"PullRequestChangedFileConnection": {kind: "OBJECT", fields: []gqlFieldDef{
		{"nodes", "[PullRequestChangedFile]"},
		{"pageInfo", "PageInfo!"},
		{"totalCount", "Int!"},
	}},
	"PullRequestChangedFile": {kind: "OBJECT", fields: []gqlFieldDef{
		{"path", "String!"},
		{"additions", "Int!"},
		{"deletions", "Int!"},
		{"changeType", "String!"},
	}},
	"PageInfo": {kind: "OBJECT", fields: []gqlFieldDef{
		{"hasNextPage", "Boolean!"},
		{"hasPreviousPage", "Boolean!"},
		{"startCursor", "String"},
		{"endCursor", "String"},
	}},
	"IssueCommentConnection": {kind: "OBJECT", fields: []gqlFieldDef{
		{"nodes", "[IssueComment]"},
		{"pageInfo", "PageInfo!"},
		{"totalCount", "Int!"},
	}},
	"IssueComment": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"body", "String!"},
		{"author", "User"},
		{"createdAt", "String!"},
	}},
	"Comment": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"body", "String!"},
		{"author", "User"},
		{"createdAt", "String!"},
	}},
	"CommentEdge": {kind: "OBJECT", fields: []gqlFieldDef{
		{"node", "Comment"},
		{"cursor", "String!"},
	}},
	"LabelConnection": {kind: "OBJECT", fields: []gqlFieldDef{
		{"nodes", "[Label]"},
		{"pageInfo", "PageInfo!"},
		{"totalCount", "Int!"},
	}},
	"Label": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
		{"name", "String!"},
		{"color", "String!"},
	}},
	"Node": {kind: "OBJECT", fields: []gqlFieldDef{
		{"id", "ID!"},
	}},
	"CreateIssuePayload": {kind: "OBJECT", fields: []gqlFieldDef{
		{"issue", "Issue"},
		{"clientMutationId", "String"},
	}},
	"CloseIssuePayload": {kind: "OBJECT", fields: []gqlFieldDef{
		{"issue", "Issue"},
		{"clientMutationId", "String"},
	}},
	"CreatePullRequestPayload": {kind: "OBJECT", fields: []gqlFieldDef{
		{"pullRequest", "PullRequest"},
		{"clientMutationId", "String"},
	}},
	"MergePullRequestPayload": {kind: "OBJECT", fields: []gqlFieldDef{
		{"pullRequest", "PullRequest"},
		{"clientMutationId", "String"},
	}},
	"AddCommentPayload": {kind: "OBJECT", fields: []gqlFieldDef{
		{"commentEdge", "CommentEdge"},
		{"comment", "Comment"},
		{"clientMutationId", "String"},
	}},
	"IssueState":       {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED"}},
	"PullRequestState": {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED", "MERGED"}},
	"MergeableState":   {kind: "ENUM", enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"RepositoryPermission": {kind: "ENUM", enumValues: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	"SearchType": {kind: "ENUM", enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}},
	"String":  {kind: "SCALAR"},
	"Int":     {kind: "SCALAR"},
	"ID":      {kind: "SCALAR"},
	"Boolean": {kind: "SCALAR"},
	"Float":   {kind: "SCALAR"},
	"__Type": {kind: "OBJECT", fields: []gqlFieldDef{
		{"kind", "String!"},
		{"name", "String"},
		{"description", "String"},
		{"fields", "[__Field]"},
		{"enumValues", "[__EnumValue]"},
		{"ofType", "__Type"},
		{"inputFields", "[__InputValue]"},
		{"interfaces", "[__Type]"},
		{"possibleTypes", "[__Type]"},
	}},
	"__Field": {kind: "OBJECT", fields: []gqlFieldDef{
		{"name", "String!"},
		{"description", "String"},
		{"args", "[__InputValue]"},
		{"type", "__Type!"},
		{"isDeprecated", "Boolean!"},
		{"deprecationReason", "String"},
	}},
	"__EnumValue": {kind: "OBJECT", fields: []gqlFieldDef{
		{"name", "String!"},
		{"description", "String"},
		{"isDeprecated", "Boolean!"},
		{"deprecationReason", "String"},
	}},
	"__InputValue": {kind: "OBJECT", fields: []gqlFieldDef{
		{"name", "String!"},
		{"description", "String"},
		{"type", "__Type!"},
		{"defaultValue", "String"},
	}},
	"__Schema": {kind: "OBJECT", fields: []gqlFieldDef{
		{"queryType", "__Type!"},
		{"mutationType", "__Type"},
		{"types", "[__Type]"},
		{"directives", "[__Directive]"},
	}},
	"__Directive": {kind: "OBJECT", fields: []gqlFieldDef{
		{"name", "String!"},
		{"description", "String"},
		{"locations", "[String]"},
		{"args", "[__InputValue]"},
	}},
}

// typeRefValue converts a type reference string into a __Type value node
// (nested maps with __refKind/__refName/__ofType, plus __typeName/__typeDef
// for named types so fields/enumValues resolve).
func typeRefValue(ref string) map[string]any {
	if strings.HasSuffix(ref, "!") {
		return map[string]any{"__refKind": "NON_NULL", "__refName": nil, "__ofType": typeRefValue(ref[:len(ref)-1])}
	}
	if strings.HasPrefix(ref, "[") && strings.HasSuffix(ref, "]") {
		return map[string]any{"__refKind": "LIST", "__refName": nil, "__ofType": typeRefValue(ref[1 : len(ref)-1])}
	}
	def, ok := gqlSchema[ref]
	kind := "OBJECT"
	if ok {
		kind = def.kind
	}
	return map[string]any{
		"__refKind": kind, "__refName": ref,
		"__typeName": ref, "__typeDef": def, "__ofType": nil,
	}
}

// typeNodeJSON converts a __Type value node to plain JSON (kind/name/ofType).
func typeNodeJSON(node map[string]any) map[string]any {
	kind, _ := node["__refKind"].(string)
	name, _ := node["__refName"].(string)
	var nameVal any
	if name != "" {
		nameVal = name
	}
	var ofType any
	if inner, ok := node["__ofType"].(map[string]any); ok && inner != nil {
		ofType = typeNodeJSON(inner)
	}
	return map[string]any{"kind": kind, "name": nameVal, "ofType": ofType}
}

// typeRefJSON converts a type reference string into a __Type object.
func typeRefJSON(ref string) map[string]any {
	return typeNodeJSON(typeRefValue(ref))
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
}

type gqlExecutor struct {
	errors []gqlError
}

func (e *gqlExecutor) errf(format string, args ...any) {
	e.errors = append(e.errors, gqlError{Message: fmt.Sprintf(format, args...)})
}

func strArg(args map[string]any, name string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func intArg(args map[string]any, name string) int64 {
	if args == nil {
		return 0
	}
	switch v := args[name].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case int:
		return int64(v)
	}
	return 0
}

func objArg(args map[string]any, name string) map[string]any {
	if args == nil {
		return nil
	}
	if v, ok := args[name]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return nil
}

// execSelection resolves a selection set against a (typeName, value) pair.
func (e *gqlExecutor) execSelection(typeName string, value any, sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range sel {
		if f.typeCond != "" {
			// Inline fragment: apply only when the type matches.
			if f.typeCond != typeName {
				continue
			}
			for k, v := range e.execSelection(typeName, value, f.children) {
				out[k] = v
			}
			continue
		}
		v, ok := e.resolveField(typeName, value, f)
		if !ok {
			// Unknown field: record an error, emit null for the field.
			e.errf("Cannot query field %q on type %q.", f.name, typeName)
			out[f.alias] = nil
			continue
		}
		out[f.alias] = v
	}
	return out
}

// resolveField resolves one field; ok=false when the field is unknown.
func (e *gqlExecutor) resolveField(typeName string, value any, f *gqlField) (any, bool) {
	switch typeName {
	case "Query":
		return e.resolveQuery(f)
	case "Mutation":
		return e.resolveMutation(f)
	case "__Type":
		return e.resolveMetaType(value, f)
	case "__Schema":
		return e.resolveMetaSchema(f)
	case "__Field":
		return e.resolveMetaField(value, f)
	case "__EnumValue":
		return e.resolveMetaEnumValue(value, f)
	case "__InputValue":
		return e.resolveMetaInputValue(value, f)
	}
	// Data types.
	switch typeName {
	case "Repository":
		return e.resolveRepository(value, f)
	case "Issue":
		return e.resolveIssue(value, f)
	case "PullRequest":
		return e.resolvePullRequest(value, f)
	case "User":
		return e.resolveUser(value, f)
	case "Ref":
		return e.resolveRef(value, f)
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection",
		"IssueCommentConnection", "LabelConnection":
		return e.resolveConnection(typeName, value, f)
	case "IssueEdge", "PullRequestEdge", "CommentEdge":
		return e.resolveEdge(typeName, value, f)
	case "PageInfo":
		return e.resolvePageInfo(value, f)
	case "IssueComment", "Comment":
		return e.resolveComment(value, f)
	case "PullRequestChangedFile":
		return e.resolveChangedFile(value, f)
	case "Label":
		return e.resolveLabel(value, f)
	case "CreateIssuePayload", "CloseIssuePayload":
		return e.resolveIssuePayload(value, f)
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		return e.resolvePRPayload(value, f)
	case "AddCommentPayload":
		return e.resolveAddCommentPayload(value, f)
	}
	return nil, false
}

// --- root ---

func (e *gqlExecutor) resolveQuery(f *gqlField) (any, bool) {
	switch f.name {
	case "__type":
		name := strArg(f.args, "name")
		def, ok := gqlSchema[name]
		if !ok {
			return nil, true
		}
		v := map[string]any{"__typeName": name, "__typeDef": def}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("__Type", v, f.children), true
	case "__schema":
		v := map[string]any{}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("__Schema", v, f.children), true
	case "viewer":
		v := map[string]any{"__kind": "User"}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("User", v, f.children), true
	case "repository":
		owner := strArg(f.args, "owner")
		name := strArg(f.args, "name")
		store.mu.RLock()
		r := store.getRepo(owner, name)
		store.mu.RUnlock()
		if r == nil {
			return nil, true
		}
		v := map[string]any{"__kind": "Repository", "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Repository", v, f.children), true
	case "node":
		id := strArg(f.args, "id")
		kind, parts, ok := decodeGlobalID(id)
		if !ok {
			return nil, true
		}
		if kind == "R" && len(parts) >= 1 {
			store.mu.RLock()
			var found *Repo
			for _, r := range store.repos {
				if r.ID == parts[0] {
					found = r
					break
				}
			}
			store.mu.RUnlock()
			if found == nil {
				return nil, true
			}
			v := map[string]any{"__kind": "Repository", "repo": found}
			if len(f.children) == 0 {
				return v, true
			}
			return e.execSelection("Repository", v, f.children), true
		}
		return nil, true
	}
	return nil, false
}

// --- mutations ---

func (e *gqlExecutor) resolveMutation(f *gqlField) (any, bool) {
	input := objArg(f.args, "input")
	switch f.name {
	case "createIssue":
		repoID := strArg(input, "repositoryId")
		title := strArg(input, "title")
		body := strArg(input, "body")
		kind, parts, ok := decodeGlobalID(repoID)
		if !ok || kind != "R" || len(parts) < 1 {
			e.errf("createIssue: unknown repositoryId")
			return nil, true
		}
		store.mu.Lock()
		var r *Repo
		for _, x := range store.repos {
			if x.ID == parts[0] {
				r = x
				break
			}
		}
		if r == nil {
			store.mu.Unlock()
			e.errf("createIssue: repository not found")
			return nil, true
		}
		is, ok2 := createIssueLocked(r, title, body, nil)
		store.mu.Unlock()
		if !ok2 {
			e.errf("createIssue: failed")
			return nil, true
		}
		v := map[string]any{"__kind": "CreateIssuePayload", "issue": is, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("CreateIssuePayload", v, f.children), true
	case "closeIssue":
		issueID := strArg(input, "issueId")
		kind, parts, ok := decodeGlobalID(issueID)
		if !ok || kind != "I" || len(parts) < 2 {
			e.errf("closeIssue: unknown issueId")
			return nil, true
		}
		store.mu.Lock()
		var r *Repo
		for _, x := range store.repos {
			if x.ID == parts[0] {
				r = x
				break
			}
		}
		if r == nil {
			store.mu.Unlock()
			e.errf("closeIssue: repository not found")
			return nil, true
		}
		is := r.IssueByNum[int(parts[1])]
		if is == nil {
			store.mu.Unlock()
			e.errf("closeIssue: issue not found")
			return nil, true
		}
		is.State = "closed"
		is.ClosedAt = nowStr()
		is.UpdatedAt = nowStr()
		is.Ver++
		store.mu.Unlock()
		v := map[string]any{"__kind": "CloseIssuePayload", "issue": is, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("CloseIssuePayload", v, f.children), true
	case "createPullRequest":
		repoID := strArg(input, "repositoryId")
		base := strArg(input, "baseRefName")
		head := strArg(input, "headRefName")
		title := strArg(input, "title")
		body := strArg(input, "body")
		kind, parts, ok := decodeGlobalID(repoID)
		if !ok || kind != "R" || len(parts) < 1 {
			e.errf("createPullRequest: unknown repositoryId")
			return nil, true
		}
		store.mu.Lock()
		var r *Repo
		for _, x := range store.repos {
			if x.ID == parts[0] {
				r = x
				break
			}
		}
		if r == nil {
			store.mu.Unlock()
			e.errf("createPullRequest: repository not found")
			return nil, true
		}
		pr, code := createPullLocked(r, title, body, head, base)
		store.mu.Unlock()
		if pr == nil {
			e.errf("createPullRequest: failed (%d)", code)
			return nil, true
		}
		v := map[string]any{"__kind": "CreatePullRequestPayload", "pr": pr, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("CreatePullRequestPayload", v, f.children), true
	case "mergePullRequest":
		prID := strArg(input, "pullRequestId")
		kind, parts, ok := decodeGlobalID(prID)
		if !ok || kind != "PR" || len(parts) < 2 {
			e.errf("mergePullRequest: unknown pullRequestId")
			return nil, true
		}
		store.mu.Lock()
		var r *Repo
		for _, x := range store.repos {
			if x.ID == parts[0] {
				r = x
				break
			}
		}
		if r == nil {
			store.mu.Unlock()
			e.errf("mergePullRequest: repository not found")
			return nil, true
		}
		pr := r.PRByNum[int(parts[1])]
		if pr == nil {
			store.mu.Unlock()
			e.errf("mergePullRequest: pull request not found")
			return nil, true
		}
		if pr.State != "open" || prHasConflict(r, pr) {
			store.mu.Unlock()
			e.errf("mergePullRequest: pull request cannot be merged")
			return nil, true
		}
		if _, ok2 := mergePullLocked(r, pr); !ok2 {
			store.mu.Unlock()
			e.errf("mergePullRequest: merge failed")
			return nil, true
		}
		store.mu.Unlock()
		v := map[string]any{"__kind": "MergePullRequestPayload", "pr": pr, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("MergePullRequestPayload", v, f.children), true
	case "addComment":
		subjectID := strArg(input, "subjectId")
		body := strArg(input, "body")
		kind, parts, ok := decodeGlobalID(subjectID)
		if !ok || len(parts) < 2 {
			e.errf("addComment: unknown subjectId")
			return nil, true
		}
		store.mu.Lock()
		var r *Repo
		for _, x := range store.repos {
			if x.ID == parts[0] {
				r = x
				break
			}
		}
		if r == nil {
			store.mu.Unlock()
			e.errf("addComment: repository not found")
			return nil, true
		}
		var c *Comment
		var issueNum int
		switch kind {
		case "I":
			issueNum = int(parts[1])
			if r.IssueByNum[issueNum] == nil {
				store.mu.Unlock()
				e.errf("addComment: issue not found")
				return nil, true
			}
			c = createCommentLocked(r, issueNum, body)
		case "PR":
			// Comments on pull requests attach to the matching issue number
			// space is separate; treat as a comment on the PR's number.
			issueNum = int(parts[1])
			c = createCommentLocked(r, issueNum, body)
		default:
			store.mu.Unlock()
			e.errf("addComment: unsupported subject")
			return nil, true
		}
		store.mu.Unlock()
		v := map[string]any{"__kind": "AddCommentPayload", "comment": c, "repo": r, "issueNum": issueNum}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("AddCommentPayload", v, f.children), true
	}
	return nil, false
}

// --- data types ---

func repoOf(value any) *Repo {
	if m, ok := value.(map[string]any); ok {
		if r, ok := m["repo"].(*Repo); ok {
			return r
		}
	}
	return nil
}

func (e *gqlExecutor) resolveRepository(value any, f *gqlField) (any, bool) {
	r := repoOf(value)
	if r == nil {
		return nil, false
	}
	switch f.name {
	case "id":
		return globalID("R", r.ID), true
	case "databaseId":
		return r.ID, true
	case "name":
		return r.Name, true
	case "nameWithOwner":
		return r.fullName(), true
	case "description":
		return r.Description, true
	case "owner":
		v := map[string]any{"__kind": "User"}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("User", v, f.children), true
	case "defaultBranchRef":
		store.mu.RLock()
		tip := r.headOID()
		store.mu.RUnlock()
		if tip == zeroOID {
			return nil, true
		}
		v := map[string]any{"__kind": "Ref", "name": r.DefaultBranch, "oid": tip}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Ref", v, f.children), true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "url":
		return "http://github.invalid/" + r.fullName(), true
	case "createdAt":
		return r.CreatedAt, true
	case "updatedAt":
		return r.UpdatedAt, true
	case "issue":
		n := int(intArg(f.args, "number"))
		store.mu.RLock()
		is := r.IssueByNum[n]
		store.mu.RUnlock()
		if is == nil {
			return nil, true
		}
		v := map[string]any{"__kind": "Issue", "issue": is, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Issue", v, f.children), true
	case "issues":
		first := int(intArg(f.args, "first"))
		after := strArg(f.args, "after")
		states := statesArg(f.args)
		orderBy := objArg(f.args, "orderBy")
		store.mu.RLock()
		list := sortedIssues(r, stateFilter(states), orderDirection(orderBy))
		store.mu.RUnlock()
		conn := paginateIssues(r, list, first, after)
		if len(f.children) == 0 {
			return conn, true
		}
		return e.execSelection("IssueConnection", conn, f.children), true
	case "pullRequest":
		n := int(intArg(f.args, "number"))
		store.mu.RLock()
		pr := r.PRByNum[n]
		store.mu.RUnlock()
		if pr == nil {
			return nil, true
		}
		v := map[string]any{"__kind": "PullRequest", "pr": pr, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("PullRequest", v, f.children), true
	case "pullRequests":
		first := int(intArg(f.args, "first"))
		store.mu.RLock()
		var list []*PR
		for _, pr := range r.PRs {
			list = append(list, pr)
		}
		store.mu.RUnlock()
		sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
		if first > 0 && len(list) > first {
			list = list[:first]
		}
		conn := map[string]any{
			"__kind": "PullRequestConnection",
			"nodes":  prNodes(r, list),
			"pageInfo": map[string]any{
				"__kind": "PageInfo", "hasNextPage": false, "hasPreviousPage": false,
				"startCursor": nil, "endCursor": nil,
			},
			"totalCount": len(r.PRs),
		}
		if len(f.children) == 0 {
			return conn, true
		}
		return e.execSelection("PullRequestConnection", conn, f.children), true
	case "labels":
		store.mu.RLock()
		names := make([]string, 0, len(r.Labels))
		for n := range r.Labels {
			names = append(names, n)
		}
		sort.Strings(names)
		var nodes []any
		for _, n := range names {
			l := r.Labels[n]
			nodes = append(nodes, map[string]any{"__kind": "Label", "label": l})
		}
		store.mu.RUnlock()
		conn := map[string]any{
			"__kind": "LabelConnection", "nodes": nodes,
			"pageInfo": map[string]any{
				"__kind": "PageInfo", "hasNextPage": false, "hasPreviousPage": false,
				"startCursor": nil, "endCursor": nil,
			},
			"totalCount": len(nodes),
		}
		if len(f.children) == 0 {
			return conn, true
		}
		return e.execSelection("LabelConnection", conn, f.children), true
	}
	return nil, false
}

func statesArg(args map[string]any) []string {
	if args == nil {
		return nil
	}
	v, ok := args["states"]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// stateFilter maps GraphQL states ([OPEN]/[CLOSED]) to a REST state filter.
func stateFilter(states []string) string {
	if len(states) == 0 {
		return "all"
	}
	hasOpen, hasClosed := false, false
	for _, s := range states {
		if s == "OPEN" {
			hasOpen = true
		}
		if s == "CLOSED" {
			hasClosed = true
		}
	}
	if hasOpen && !hasClosed {
		return "open"
	}
	if hasClosed && !hasOpen {
		return "closed"
	}
	return "all"
}

func orderDirection(orderBy map[string]any) string {
	if orderBy == nil {
		return "asc"
	}
	if d, ok := orderBy["direction"].(string); ok && d == "DESC" {
		return "desc"
	}
	return "asc"
}

func cursorFor(n int) string { return b64encode([]byte("cursor:" + strconv.Itoa(n))) }

func cursorNumber(cursor string) (int, bool) {
	b, err := b64decode(cursor)
	if err != nil {
		return 0, false
	}
	s := string(b)
	if !strings.HasPrefix(s, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

// paginateIssues applies first/after cursor pagination (creation order).
func paginateIssues(r *Repo, list []*Issue, first int, after string) map[string]any {
	start := 0
	if after != "" {
		if n, ok := cursorNumber(after); ok {
			for i, is := range list {
				if is.Number == n {
					start = i + 1
					break
				}
			}
		}
	}
	end := len(list)
	if first > 0 && start+first < end {
		end = start + first
	}
	page := list[start:end]
	var nodes []any
	for _, is := range page {
		nodes = append(nodes, map[string]any{"__kind": "Issue", "issue": is, "repo": r})
	}
	var endCursor any
	hasNext := end < len(list)
	if len(page) > 0 {
		endCursor = cursorFor(page[len(page)-1].Number)
	}
	return map[string]any{
		"__kind": "IssueConnection",
		"nodes":  nodes,
		"pageInfo": map[string]any{
			"__kind": "PageInfo", "hasNextPage": hasNext, "hasPreviousPage": start > 0,
			"startCursor": nil, "endCursor": endCursor,
		},
		"totalCount": len(list),
	}
}

func prNodes(r *Repo, list []*PR) []any {
	var nodes []any
	for _, pr := range list {
		nodes = append(nodes, map[string]any{"__kind": "PullRequest", "pr": pr, "repo": r})
	}
	return nodes
}

func (e *gqlExecutor) resolveIssue(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	is, _ := m["issue"].(*Issue)
	r, _ := m["repo"].(*Repo)
	if is == nil {
		return nil, false
	}
	switch f.name {
	case "id":
		return globalID("I", r.ID, int64(is.Number)), true
	case "databaseId":
		return is.Seq, true
	case "number":
		return is.Number, true
	case "title":
		return is.Title, true
	case "body":
		return is.Body, true
	case "state":
		if is.State == "open" {
			return "OPEN", true
		}
		return "CLOSED", true
	case "author":
		v := map[string]any{"__kind": "User"}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("User", v, f.children), true
	case "createdAt":
		return is.CreatedAt, true
	case "updatedAt":
		return is.UpdatedAt, true
	case "closedAt":
		if is.State == "closed" {
			return is.ClosedAt, true
		}
		return nil, true
	case "url":
		return "http://github.invalid/" + r.fullName() + "/issues/" + strconv.Itoa(is.Number), true
	case "repository":
		v := map[string]any{"__kind": "Repository", "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Repository", v, f.children), true
	case "comments":
		store.mu.RLock()
		comments := r.Comments[is.Number]
		store.mu.RUnlock()
		var nodes []any
		for _, c := range comments {
			nodes = append(nodes, map[string]any{"__kind": "IssueComment", "comment": c, "repo": r})
		}
		conn := map[string]any{
			"__kind": "IssueCommentConnection", "nodes": nodes,
			"pageInfo": map[string]any{
				"__kind": "PageInfo", "hasNextPage": false, "hasPreviousPage": false,
				"startCursor": nil, "endCursor": nil,
			},
			"totalCount": len(nodes),
		}
		if len(f.children) == 0 {
			return conn, true
		}
		return e.execSelection("IssueCommentConnection", conn, f.children), true
	case "labels":
		var nodes []any
		for _, name := range is.Labels {
			if l, ok := r.Labels[name]; ok {
				nodes = append(nodes, map[string]any{"__kind": "Label", "label": l})
			}
		}
		conn := map[string]any{
			"__kind": "LabelConnection", "nodes": nodes,
			"pageInfo": map[string]any{
				"__kind": "PageInfo", "hasNextPage": false, "hasPreviousPage": false,
				"startCursor": nil, "endCursor": nil,
			},
			"totalCount": len(nodes),
		}
		if len(f.children) == 0 {
			return conn, true
		}
		return e.execSelection("LabelConnection", conn, f.children), true
	}
	return nil, false
}

func (e *gqlExecutor) resolvePullRequest(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	pr, _ := m["pr"].(*PR)
	r, _ := m["repo"].(*Repo)
	if pr == nil {
		return nil, false
	}
	switch f.name {
	case "id":
		return globalID("PR", r.ID, int64(pr.Number)), true
	case "databaseId":
		return pr.Seq, true
	case "number":
		return pr.Number, true
	case "title":
		return pr.Title, true
	case "body":
		return pr.Body, true
	case "state":
		if pr.State == "open" {
			return "OPEN", true
		}
		if pr.Merged {
			return "MERGED", true
		}
		return "CLOSED", true
	case "merged":
		return pr.Merged, true
	case "mergeable":
		store.mu.RLock()
		conflict := prHasConflict(r, pr)
		store.mu.RUnlock()
		if conflict {
			return "CONFLICTING", true
		}
		return "MERGEABLE", true
	case "headRefName":
		return pr.HeadRef, true
	case "baseRefName":
		return pr.BaseRef, true
	case "headRef", "baseRef":
		refName := pr.HeadRef
		if f.name == "baseRef" {
			refName = pr.BaseRef
		}
		store.mu.RLock()
		oid, ok := r.Refs["refs/heads/"+refName]
		store.mu.RUnlock()
		if !ok {
			return nil, true
		}
		v := map[string]any{"__kind": "Ref", "name": refName, "oid": oid}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Ref", v, f.children), true
	case "files":
		first := int(intArg(f.args, "first"))
		store.mu.RLock()
		baseTree, headTree, ok := prTrees(r, pr)
		var changes []fileChange
		if ok {
			changes = diffTrees(r, baseTree, headTree)
		}
		store.mu.RUnlock()
		if first > 0 && len(changes) > first {
			changes = changes[:first]
		}
		var nodes []any
		for _, ch := range changes {
			nodes = append(nodes, map[string]any{
				"__kind": "PullRequestChangedFile",
				"path": ch.Path, "additions": ch.Additions, "deletions": ch.Deletions,
			})
		}
		conn := map[string]any{
			"__kind": "PullRequestChangedFileConnection", "nodes": nodes,
			"pageInfo": map[string]any{
				"__kind": "PageInfo", "hasNextPage": false, "hasPreviousPage": false,
				"startCursor": nil, "endCursor": nil,
			},
			"totalCount": len(nodes),
		}
		if len(f.children) == 0 {
			return conn, true
		}
		return e.execSelection("PullRequestChangedFileConnection", conn, f.children), true
	case "author":
		v := map[string]any{"__kind": "User"}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("User", v, f.children), true
	case "createdAt":
		return pr.CreatedAt, true
	case "updatedAt":
		return pr.UpdatedAt, true
	case "url":
		return "http://github.invalid/" + r.fullName() + "/pull/" + strconv.Itoa(pr.Number), true
	case "repository":
		v := map[string]any{"__kind": "Repository", "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Repository", v, f.children), true
	}
	return nil, false
}

func (e *gqlExecutor) resolveUser(value any, f *gqlField) (any, bool) {
	switch f.name {
	case "id":
		return globalID("U", viewerID), true
	case "login":
		return viewerLogin, true
	case "name":
		return viewerLogin, true
	case "url":
		return "http://github.invalid/" + viewerLogin, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveRef(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	name, _ := m["name"].(string)
	oid, _ := m["oid"].([20]byte)
	switch f.name {
	case "id":
		return globalID("REF", 0), true
	case "name":
		return name, true
	case "prefix":
		return "refs/heads/", true
	case "target":
		v := map[string]any{"__kind": "GitObject", "oid": oid}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("GitObject", v, f.children), true
	}
	return nil, false
}

func (e *gqlExecutor) resolveConnection(typeName string, value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "nodes":
		if nodes, ok := m["nodes"].([]any); ok {
			// Resolve each node through its own type's selection set.
			var out []any
			for _, n := range nodes {
				nm, ok := n.(map[string]any)
				if !ok {
					out = append(out, n)
					continue
				}
				kind, _ := nm["__kind"].(string)
				if len(f.children) == 0 {
					out = append(out, stripMeta(nm))
					continue
				}
				out = append(out, e.execSelection(kind, n, f.children))
			}
			return out, true
		}
		return []any{}, true
	case "edges":
		nodes, _ := m["nodes"].([]any)
		var out []any
		for _, n := range nodes {
			nm, _ := n.(map[string]any)
			cursor := ""
			if nm != nil {
				switch nm["__kind"] {
				case "Issue":
					if is, ok := nm["issue"].(*Issue); ok {
						cursor = cursorFor(is.Number)
					}
				case "PullRequest":
					if pr, ok := nm["pr"].(*PR); ok {
						cursor = cursorFor(pr.Number)
					}
				}
			}
			edge := map[string]any{"__kind": edgeType(typeName), "node": n, "cursor": cursor}
			if len(f.children) == 0 {
				out = append(out, edge)
				continue
			}
			out = append(out, e.execSelection(edgeType(typeName), edge, f.children))
		}
		return out, true
	case "pageInfo":
		pi, _ := m["pageInfo"].(map[string]any)
		if pi == nil {
			return nil, true
		}
		if len(f.children) == 0 {
			return stripMeta(pi), true
		}
		return e.execSelection("PageInfo", pi, f.children), true
	case "totalCount":
		if n, ok := m["totalCount"].(int); ok {
			return n, true
		}
		return 0, true
	}
	return nil, false
}

func edgeType(conn string) string {
	switch conn {
	case "IssueConnection", "IssueCommentConnection":
		return "IssueEdge"
	case "PullRequestConnection":
		return "PullRequestEdge"
	case "PullRequestChangedFileConnection":
		return "PullRequestEdge"
	case "LabelConnection":
		return "IssueEdge"
	}
	return "IssueEdge"
}

func stripMeta(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if strings.HasPrefix(k, "__") {
			continue
		}
		out[k] = v
	}
	return out
}

func (e *gqlExecutor) resolveEdge(typeName string, value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "node":
		n, _ := m["node"].(map[string]any)
		if n == nil {
			return nil, true
		}
		kind, _ := n["__kind"].(string)
		if len(f.children) == 0 {
			return stripMeta(n), true
		}
		return e.execSelection(kind, n, f.children), true
	case "cursor":
		return m["cursor"], true
	}
	return nil, false
}

func (e *gqlExecutor) resolvePageInfo(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	if v, ok := m[f.name]; ok {
		return v, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveComment(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	c, _ := m["comment"].(*Comment)
	r, _ := m["repo"].(*Repo)
	if c == nil {
		return nil, false
	}
	switch f.name {
	case "id":
		return globalID("C", r.ID, 0, c.ID), true
	case "body":
		return c.Body, true
	case "author":
		v := map[string]any{"__kind": "User"}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("User", v, f.children), true
	case "createdAt":
		return c.CreatedAt, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveChangedFile(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "path":
		return m["path"], true
	case "additions":
		return m["additions"], true
	case "deletions":
		return m["deletions"], true
	case "changeType":
		return "MODIFIED", true
	}
	return nil, false
}

func (e *gqlExecutor) resolveLabel(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	l, _ := m["label"].(*Label)
	if l == nil {
		return nil, false
	}
	switch f.name {
	case "id":
		return globalID("L", l.ID), true
	case "name":
		return l.Name, true
	case "color":
		return l.Color, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveIssuePayload(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "issue":
		is, _ := m["issue"].(*Issue)
		r, _ := m["repo"].(*Repo)
		if is == nil {
			return nil, true
		}
		v := map[string]any{"__kind": "Issue", "issue": is, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Issue", v, f.children), true
	case "clientMutationId":
		return nil, true
	}
	return nil, false
}

func (e *gqlExecutor) resolvePRPayload(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "pullRequest":
		pr, _ := m["pr"].(*PR)
		r, _ := m["repo"].(*Repo)
		if pr == nil {
			return nil, true
		}
		v := map[string]any{"__kind": "PullRequest", "pr": pr, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("PullRequest", v, f.children), true
	case "clientMutationId":
		return nil, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveAddCommentPayload(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "commentEdge":
		c, _ := m["comment"].(*Comment)
		r, _ := m["repo"].(*Repo)
		issueNum, _ := m["issueNum"].(int)
		edge := map[string]any{
			"__kind": "CommentEdge",
			"node":   map[string]any{"__kind": "Comment", "comment": c, "repo": r, "issueNum": issueNum},
			"cursor": cursorFor(int(c.ID)),
		}
		if len(f.children) == 0 {
			return edge, true
		}
		return e.execSelection("CommentEdge", edge, f.children), true
	case "comment":
		c, _ := m["comment"].(*Comment)
		r, _ := m["repo"].(*Repo)
		v := map[string]any{"__kind": "Comment", "comment": c, "repo": r}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("Comment", v, f.children), true
	case "clientMutationId":
		return nil, true
	}
	return nil, false
}

// --- introspection resolvers ---

func (e *gqlExecutor) resolveMetaType(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	// Wrapper / named node produced by typeRefValue.
	if refKind, isRef := m["__refKind"].(string); isRef {
		switch f.name {
		case "kind":
			return refKind, true
		case "name":
			return m["__refName"], true
		case "description":
			return nil, true
		case "ofType":
			inner, _ := m["__ofType"].(map[string]any)
			if inner == nil {
				return nil, true
			}
			if len(f.children) == 0 {
				return typeNodeJSON(inner), true
			}
			return e.execSelection("__Type", inner, f.children), true
		}
		// fields / enumValues fall through to the named-type handling below.
	}
	name, _ := m["__typeName"].(string)
	def, _ := m["__typeDef"].(*gqlTypeDef)
	switch f.name {
	case "kind":
		if def != nil {
			return def.kind, true
		}
		return "OBJECT", true
	case "name":
		return name, true
	case "description":
		return nil, true
	case "fields":
		if def == nil {
			return nil, true
		}
		var out []any
		for _, fd := range def.fields {
			out = append(out, map[string]any{
				"__kind": "field", "name": fd.name, "typeRef": fd.typ,
			})
		}
		if len(f.children) == 0 {
			return out, true
		}
		var resolved []any
		for _, item := range out {
			resolved = append(resolved, e.execSelection("__Field", item, f.children))
		}
		return resolved, true
	case "enumValues":
		if def == nil || def.kind != "ENUM" {
			return nil, true
		}
		var out []any
		for _, v := range def.enumValues {
			out = append(out, map[string]any{"__kind": "enumValue", "name": v})
		}
		if len(f.children) == 0 {
			return out, true
		}
		var resolved []any
		for _, item := range out {
			resolved = append(resolved, e.execSelection("__EnumValue", item, f.children))
		}
		return resolved, true
	case "ofType":
		return nil, true
	case "inputFields":
		return nil, true
	case "interfaces":
		return []any{}, true
	case "possibleTypes":
		return nil, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveMetaField(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "name":
		return m["name"], true
	case "description":
		return nil, true
	case "args":
		return []any{}, true
	case "type":
		ref, _ := m["typeRef"].(string)
		node := typeRefValue(ref)
		if len(f.children) == 0 {
			return typeNodeJSON(node), true
		}
		return e.execSelection("__Type", node, f.children), true
	case "isDeprecated":
		return false, true
	case "deprecationReason":
		return nil, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveMetaEnumValue(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "name":
		return m["name"], true
	case "description":
		return nil, true
	case "isDeprecated":
		return false, true
	case "deprecationReason":
		return nil, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveMetaInputValue(value any, f *gqlField) (any, bool) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch f.name {
	case "name":
		return m["name"], true
	case "description":
		return nil, true
	case "type":
		return typeRefJSON("String"), true
	case "defaultValue":
		return nil, true
	}
	return nil, false
}

func (e *gqlExecutor) resolveMetaSchema(f *gqlField) (any, bool) {
	switch f.name {
	case "queryType":
		v := map[string]any{"__typeName": "Query", "__typeDef": gqlSchema["Query"]}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("__Type", v, f.children), true
	case "mutationType":
		v := map[string]any{"__typeName": "Mutation", "__typeDef": gqlSchema["Mutation"]}
		if len(f.children) == 0 {
			return v, true
		}
		return e.execSelection("__Type", v, f.children), true
	case "types":
		names := make([]string, 0, len(gqlSchema))
		for n := range gqlSchema {
			names = append(names, n)
		}
		sort.Strings(names)
		var out []any
		for _, n := range names {
			v := map[string]any{"__typeName": n, "__typeDef": gqlSchema[n]}
			if len(f.children) == 0 {
				out = append(out, v)
			} else {
				out = append(out, e.execSelection("__Type", v, f.children))
			}
		}
		return out, true
	case "directives":
		return []any{}, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func handleGraphQL(w http.ResponseWriter, req *http.Request) {
	if !requireAuth(w, req) {
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}
	// Batched operations: a JSON array of {query, variables}.
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var batch []map[string]any
		if err := json.Unmarshal(body, &batch); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
		results := make([]any, 0, len(batch))
		for _, item := range batch {
			results = append(results, executeGraphQL(item))
		}
		writeJSONResp(w, http.StatusOK, results)
		return
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	writeJSONResp(w, http.StatusOK, executeGraphQL(reqBody))
}

// executeGraphQL runs one {query, variables} operation document.
func executeGraphQL(reqBody map[string]any) map[string]any {
	query, _ := reqBody["query"].(string)
	vars, _ := reqBody["variables"].(map[string]any)
	if vars == nil {
		vars = map[string]any{}
	}
	toks, err := lexGraphQL(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: err.Error()}}}
	}
	parser := &gqlParser{toks: toks, vars: vars}
	ops, err := parser.parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: err.Error()}}}
	}
	if len(ops) == 0 {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: "no operations in document"}}}
	}
	// The suite sends one operation per document; run the first.
	op := ops[0]
	ex := &gqlExecutor{}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	data := ex.execSelection(rootType, nil, op.sel)
	resp := map[string]any{"data": data}
	if len(ex.errors) > 0 {
		resp["errors"] = ex.errors
	}
	return resp
}
