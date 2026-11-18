# github-core-clone-01 entry: forge

A standalone, GitHub-compatible git forge in one process, built for
[the contest](https://make.fun/bounty/github-core-clone-01). One static Go
binary serves the four surfaces agents already speak — git smart HTTP (v0
and v2), REST v3, GraphQL v4 and MCP over streamable HTTP — from a single
in-memory store. No database, no UI, no external service.

## Run it

```sh
node main.ts
```

`main.ts` (Node 22, standard library only) spawns the vendored `forge`
binary next to it. The binary binds an ephemeral port, registers it with
the harness env (`POST $MAKEFUN_ENV_URL/register {"port": N}`), serves,
polls `GET $MAKEFUN_ENV_URL/done` until the trial ends, then exits 0.
Without `MAKEFUN_ENV_URL` it serves locally on `$PORT` (default 18080)
instead — handy for a smoke test:

```sh
PORT=18080 ./forge &
curl localhost:18080/healthz
```

## Layout

- `main.ts`: the entrypoint the harness runs; spawns `forge`.
- `forge`: the static Linux amd64 binary (CGO_ENABLED=0, stripped,
  6.6 MB), built from the Go sources below with Go 1.25.4.
- `main.go`: HTTP routing and the register / poll-`/done` lifecycle.
- `store.go`: the in-memory store (repos, refs, objects, issues, PRs,
  labels) and git object primitives.
- `git.go`: pkt-line, pack parse/build, object collection.
- `gitproto.go`: smart-HTTP advertisements, upload-pack (v0/v2, shallow,
  deepen), receive-pack.
- `rest.go`: the REST v3 surface.
- `graphql.go`: the GraphQL v4 surface.
- `mcp.go`: the MCP endpoint (initialize, tools/list, tools/call).
- `go.mod`: the Go module (`forge`, no dependencies).

Rebuilding the binary from source:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o forge .
```

## The surface

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"status":"ok"}`: the env's boot probe |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4 |
| `POST /mcp` | Model Context Protocol over streamable HTTP |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

Every API call carries `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`; the
bearer names the scenario viewer (`adaiversen`). A request with no token
is 401; a repository under another login is 404 for this token.

## Performance notes

The hot paths are built for the timed workloads: the receive-pack and
upload-pack advertisements are cached per repo version, depth-1 clone
packs are cached, issue GET responses are pre-serialized, and pack
collection excludes everything reachable from the client's haves (so an
incremental fetch never re-sends a held object) while still traversing
through a held shallow boundary so a deepen request delivers the next
generation.

## License

MIT. See [LICENSE](LICENSE).
