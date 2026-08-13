# CLAUDE.md

guidance for future claude code sessions working in this repo. focuses on the
non-obvious — conventions, gotchas, and deploy mechanics that aren't visible
from the source alone.

## what this is

torrent-manager: a small system that ingests rss torrent feeds, indexes the
items in sqlite, matches them against a yaml watchlist, and enqueues matches
(plus user-driven manual picks) into rtorrent via a seedbox-side bridge.
three binaries cooperate over mtls.

## components

- **cmd/tmgr** — k3s daemonset. polls feeds, indexes items via seedctl,
  evaluates rules, enqueues matches. items matching **no** rule are logged
  at debug and skipped entirely (`run.go`, `item_no_match`) — tmgr never
  enqueues them. the claude classifier is *not* a no-rule fallback: it only
  runs for items that **already matched** a rule whose season couldn't be
  regex-extracted, purely to resolve show/season for the save_dir
  (`resolveShowSeasonAI`). so every enqueue with an empty `matched_rule`
  came from tmweb by hand, never from tmgr.
- **cmd/seedctl** — runs natively on the seedbox (seedbox.example.com). https
  on :18443. wraps rtorrent (scgi). owns the sqlite items index.
- **cmd/tmweb** — k3s web ui (port :8080, lan-only). htmx + sse. serves
  search + manual enqueue with an ai-assisted "no rule" path.

leaf packages under `internal/` follow standard go layout. quick reference:

| package           | role                                              |
| ----------------- | ------------------------------------------------- |
| aiclassify        | anthropic sdk wrapper. `PickDir` (opus 4.7) for the modal classify; `Classify` (opus 4.7) for tmgr background fallback. |
| api               | shared request/response types across binaries.    |
| feed              | rss item types, info-hash helpers.                |
| itemsdb           | sqlite items index + FTS5 search. requires `-tags=sqlite_fts5`. |
| rtorrent          | scgi client (seedctl only).                       |
| savedir           | derive save_dir from rule + title.                |
| tlsx              | mtls client/server config helpers.                |
| torrent           | bencode parser (`anacrolix/torrent/metainfo`).    |
| watchlist         | yaml loader + compiler.                           |

## deploy topology

- two raspberry pis on the lan: `node-1.local` + `node-2.local` (k3s cluster).
- kubeconfig at `~/.kube/rpi-sandbox.yaml`.
- one seedbox: `seedbox.example.com` (native seedctl binary, no container).
- mtls between every k3s pod (tmweb, tmgr) and seedctl on :18443.

## build + test

- `make build` — host-arch all binaries.
- `make build-tmweb` / `make build-tmgr` — arm64 cross-builds for k3s.
- `make build-seedctl` — amd64 cross-build for the seedbox.
- `make test` — full project test.
- **direct go invocations need the fts5 tag**: `go test -tags=sqlite_fts5 ./...`.
  forgetting it produces opaque "no such module: fts5" errors at runtime.
- `make lint` — go vet.
- `make certs` — regenerate ca + server + client certs (idempotent).

## deploy

- **tmweb**: `make image-import-tmweb` — podman build (arm64) → docker-
  archive tar in `dist/` → scp to both pis → `k3s ctr images import`.
  passwordless sudo is configured on both pis via
  `/etc/sudoers.d/k3s-ctr-import` (scoped to the two tarball paths under
  `/tmp/`), so no sudo prompt is involved. then rollout:

  ```
  kubectl --kubeconfig=$HOME/.kube/rpi-sandbox.yaml -n torrent-manager \
    rollout restart deploy/tmweb
  kubectl --kubeconfig=$HOME/.kube/rpi-sandbox.yaml -n torrent-manager \
    rollout status deploy/tmweb --timeout=90s
  ```

- **tmgr**: `make image-import` then rollout the deployment.
- **seedctl**: `make deploy-seedhost` — scp+ssh, builds the binary on the
  seedbox via go (avoids cgo cross-compile issues), drops it at
  `/home/seedbox/bin/seedctl` along with the certs.
- **watchlist**: the real watchlist lives in `private/watchlist.yaml` (the
  tracked `deploy/k8s/configmap.yaml` is a scrubbed placeholder). it reaches
  the cluster as the `tmgr-watchlist` configmap, mounted at
  `/etc/tmgr/watchlist.yaml`. the tmgr CronJob is `*/5 * * * *` and each run
  spawns a fresh pod that mounts the current configmap — so updating the
  configmap is the whole deploy; the new rules take effect on the next tick,
  no rollout needed. apply with:
  ```
  kubectl --kubeconfig=$HOME/.kube/rpi-sandbox.yaml create configmap tmgr-watchlist \
    --from-file=watchlist.yaml=private/watchlist.yaml \
    --dry-run=client -o yaml -n torrent-manager | \
  kubectl --kubeconfig=$HOME/.kube/rpi-sandbox.yaml apply -f -
  ```
  **ALWAYS deploy immediately after editing the watchlist** — an edit that
  isn't pushed to the cluster is useless; tmgr only ever reads the configmap.

### deploying when kubectl can't reach the cluster (this workstation)

on this mac, macOS sequoia's Local Network privacy (a TCC permission, NOT the
application firewall — that's off) blocks **third-party / non-apple binaries**
from reaching the local network. it hits any homebrew binary — `kubectl`,
`python3`, etc. — which all die with `connect: no route to host` (EHOSTUNREACH)
on BOTH the v4 LAN subnet and the on-link v6 `/64`, while apple-signed binaries
(`/usr/bin/curl`, `ping`, `ping6`) are exempt and reach the same
`https://<node-ip>:6443` fine. ipv6 itself is healthy — ping6 + curl reach the
pis over v6; the "no route to host" is a synthesized denial, not a routing
fault. **the fix that actually worked (2026-06-15): restart the tmux server**
— no settings change, no permission granted. the long-lived tmux server (old,
low pid) was holding a stale "denied" local-network state for its whole
lifetime, and every binary it spawned inherited it; a fresh tmux server
cleared it and kubectl/python3 reached the LAN again. (exact mechanism —
cached TCC decision vs other per-process state — not confirmed; the
empirical, repeatable fix is the restart.) if a restart isn't enough, the
curl workaround below doesn't need kubectl networking at all: kubectl
sub-commands that are purely local still work (`config view --raw`,
`create ... --dry-run=client`), so generate artifacts locally and ship them
with curl + the client certs pulled out of the kubeconfig:
```
KC=$HOME/.kube/rpi-sandbox.yaml; TMP=$(mktemp -d)
kubectl --kubeconfig=$KC config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > $TMP/ca.crt
kubectl --kubeconfig=$KC config view --raw -o jsonpath='{.users[0].user.client-certificate-data}'   | base64 -d > $TMP/client.crt
kubectl --kubeconfig=$KC config view --raw -o jsonpath='{.users[0].user.client-key-data}'           | base64 -d > $TMP/client.key
kubectl create configmap tmgr-watchlist --from-file=watchlist.yaml=private/watchlist.yaml \
  --dry-run=client -o json -n torrent-manager > $TMP/cm.json
# server-side apply (json is valid yaml for apply-patch); node ipv4, not the .local name
curl -sS --cacert $TMP/ca.crt --cert $TMP/client.crt --key $TMP/client.key -X PATCH \
  -H "Content-Type: application/apply-patch+yaml" \
  "https://10.0.0.189:6443/api/v1/namespaces/torrent-manager/configmaps/tmgr-watchlist?fieldManager=deploy&force=true" \
  --data-binary @$TMP/cm.json
```
first thing to try (so plain kubectl works again): **restart the tmux server**
(`tmux kill-server`, or detach and start a fresh one) and re-run kubectl. that
alone fixed it on 2026-06-15 with no settings change. if it persists, then look
at system settings → privacy & security → local network for the terminal/tmux,
or run kubectl outside tmux. confirm the symptom any time with a one-liner:
a homebrew binary (`python3 -c "import socket; socket.create_connection(('10.0.0.189',6443),5)"`)
fails with EHOSTUNREACH while `/usr/bin/curl https://10.0.0.189:6443/healthz -k`
succeeds.

## conventions

- **all human-readable text is lowercase** in templates, logs, cli output,
  and commit messages, unless quoting case-sensitive content. global rule
  from user's claude.md.
- **commit messages**: imperative; conventional-ish prefixes
  (`feat(tmweb):`, `fix(itemsdb):`, `ui(tmweb):`, `docs:`, `deploy:`).
  footer: `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>` when
  authored via claude code.
- **no merge commits** — fast-forward or rebase only.
- **test-then-fix** for bugs — write a failing test first, watch it fail,
  then implement the fix.

## non-obvious things

### security + secrets

- **mtls everywhere internal**. certs live under `/certs/` (gitignored).
  client builds via `internal/tlsx.ClientConfig(caPath, certPath, keyPath)`.
- **`/private/` is gitignored** — tracker passkeys + cookies live there.
  never commit. fixture passkeys in tests are fake.
- **per-feed cookies** — each watchlist feed declares a `cookie_env` (e.g.
  `COOKIE_TRACKER_A`). the `tmgr-cookies` k8s secret is mounted into both tmgr
  and tmweb pods.
- **cookie redaction in logs**: when a log line touches a cookie value,
  log only `len + head8 + tail8` when `len >= 24`, never the full value.
- **ANTHROPIC_API_KEY**: optional. log presence/absence only, never the
  value. without it tmweb still works for rule-matched + movies + other
  paths; the no-rule tv flow hides the ✨ icon and falls through to
  manual entry.

### templates + assets

- html templates are `html/template` with `{{define "_x.html"}}` partials,
  parsed via `template.ParseFS` against an `//go:embed templates/*.html`
  filesystem. partials live in `cmd/tmweb/templates/_*.html`. add new
  partials to the embed list (covered by glob) AND to the relevant
  template set's parse step in `cmd/tmweb/render.go`.
- **htmx 2.0.4 vendored** at `cmd/tmweb/static/htmx.min.js`, served from
  the embedded fs at `/static/`. don't reach for the cdn.
- static assets (`static/`) are also `//go:embed`-ed — binary is fully
  self-contained, no separate asset deployment.

### htmx 2.x gotcha — ancestor-fallback dispatch

when a requesting element is removed from the DOM by its own swap (e.g.
a nested button inside a parent that the swap replaces), htmx 2.x
explicitly re-fires `htmx:afterRequest` directly on the closest
still-attached ancestor. so `event.target === this` checks on a form's
`hx-on::after-request` will see the form as the target even when a
nested button issued the request. filter on `event.detail.xhr.responseURL`
or `event.detail.requestConfig.path` instead. see `_enqueue_form.html`
for the working pattern.

### sse-based classify flow

`/api/enqueue-classify` is an sse stream, not a single response. server
emits real progress events tied to actual work:

- `event: step` / `data: fetching|parsing|caching|querying`
- `event: meta` / `data: <hx-swap-oob dl>` — OOB swap of `#torrent-meta`
- `event: form` / `data: <form-row-block html>` — terminal payload

helpers: `writeSSEEvent` in `cmd/tmweb/main.go` (one event per call,
flushes immediately, multi-line data preserved). `renderTemplate` to
materialise a template into a string for embedding in `data:`. client
side: `startClassify` in `cmd/tmweb/templates/layout.html` (single
in-flight EventSource per page, state-class toggling on `.step` items).

### torrent cache

`cmd/tmweb/torrentcache.go` — in-memory ttl cache bridging the classify
→ enqueue handoff so we don't hit the tracker twice for the same item.

- keyed on `(feed, guid)`.
- value carries `{Bytes, Meta, InfoHash}`.
- 24-hour ttl, 50-entry cap. lazy expiry on read; size-bound eviction
  picks the soonest-to-expire entry.
- populated by classify via `ensureTorrentCached`.
- consumed by enqueueDo (so a cache hit means one tracker fetch total).
- eagerly evicted after a successful seedctl post.
- both modals (rule-matched + no-rule) probe the cache on open and
  surface size + contents rows inline on a hit.

### path display

roots.TV + roots.Movies share a parent (e.g. `/home/seedbox/t/`). modals
display paths relative to that common root (`tv/severance/season2`) and
the server expands them back to absolute on POST. see `computeCommonRoot`,
`stripPrefix`, `expandPrefix` in `cmd/tmweb/main.go`.

### item ordering

tmweb mirrors rss feed order: `first_seen_at DESC, id ASC`. covering
index `items_firstseen` is declared in `internal/itemsdb`. when search
falls through to FTS, ordering is `bm25(items_fts), i.first_seen_at DESC,
i.id ASC` (bm25 first, then date as a tiebreaker).

## local dev tips

- itemsdb-touching test: `go test -tags=sqlite_fts5 ./internal/itemsdb/...`.
- aiclassify-touching test: stub the anthropic api via
  `option.WithBaseURL(srv.URL)` against an httptest server. see
  `internal/aiclassify/pickdir_test.go` for the shape.
- `httptest.NewRecorder()` implements `http.Flusher` (as a no-op), so sse
  handlers work fine against it — the entire response is buffered and
  you can read events out of `rr.Body.String()`.
- when adding a tmweb test that needs a real `.torrent` payload, build it
  with `anacrolix/torrent/bencode` + `metainfo.Info` + `metainfo.MetaInfo`
  (existing tests in `cmd/tmweb/main_test.go` show the pattern).
- ad-hoc scripts that import `internal/...` (e.g. to load + compile
  `private/watchlist.yaml` end-to-end) MUST live inside the module tree.
  `go run /tmp/foo.go` importing `internal/watchlist` fails with "use of
  internal package not allowed". drop a throwaway `cmd/_check/main.go`,
  run `go run ./cmd/_check/`, then `rm -rf cmd/_check`. `go run -` with a
  heredoc also doesn't work — `go run` needs a file path or a package, not
  stdin.
- ssh-wrapped sqlite queries against the seedbox need inverted quote
  nesting: outer `ssh host "..."`, inner sqlite literals in single quotes.
  `sqlite3 db "SELECT ... WHERE t LIKE '%foo%'"`. double-quoted string
  literals inside the sql get parsed as column names ("no such column: %foo%").
  see also `~/.claude/.../memory/feedback_sqlite_path_on_tiny.md` for the
  binary path (`~/bin/sqlite3 ~/etc/seedctl/items.db`).
- **`items` table columns** (so you don't guess and hit "no such column"):
  `id, feed_name, guid, title, link, info_hash, matched_rule,
  first_seen_at, last_seen_at, total_bytes, file_count, dir_count`. note
  it's `feed_name`, NOT `feed`. the `first_seen_at`/`last_seen_at` are unix
  epoch ints. FTS mirror is `items_fts(title)`. canonical schema +
  indexes live in `internal/itemsdb/itemsdb.go` — check there or run
  `.schema items` before writing a query.

## layout

```
cmd/
  tmgr/           — k3s daemon, feed polling + rule matching
  tmweb/          — k3s web ui
    static/       — vendored htmx + app.css (embedded)
    templates/    — html templates (embedded)
  seedctl/        — seedbox binary, rtorrent + items index
internal/
  aiclassify/     — anthropic sdk wrapper
  api/            — shared request/response types
  feed/           — rss types, hash helpers
  itemsdb/        — sqlite + FTS5
  rtorrent/       — scgi client (seedctl only)
  savedir/        — derive save_dir from rule + title
  tlsx/           — mtls client/server helpers
  torrent/        — bencode parser
  watchlist/      — yaml loader + compiler
deploy/
  k8s/            — kustomize manifests for the cluster
  seedhost/       — seedctl install script
```
