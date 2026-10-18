# torrent-manager

automated rss → rtorrent pipeline. a k3s CronJob polls a private tracker's rss feeds every 5 minutes, matches items against a regex watchlist, and posts matching `.torrent` files to a tiny mtls-protected http server running alongside rtorrent on a remote seed host.

- design: see `[[design-spec]]` in the obsidian agent-notes vault (`torrent-manager/2026-05-18/design-spec`)
- plan: see `[[impl-plan]]` in the obsidian agent-notes vault (`torrent-manager/2026-05-18/impl-plan`)

## components

- `cmd/seedctl/` — long-lived mtls http server on the seed host. proxies a narrow API (`POST /v1/torrents`, `GET /v1/torrents/{hash}`, `GET /healthz`) into rtorrent's xmlrpc-over-scgi.
- `cmd/tmgr/` — one-shot CronJob binary. polls feeds, matches, dedups via seedctl, enqueues new items.
- `cmd/tmnotify/` — long-running k3s deployment. follows seedctl's completion stream (`GET /v1/completions/stream`, sse over mtls) and publishes a "download complete" message to an mqtt broker (e.g. home assistant) for every enqueued torrent that finishes.
- `internal/{api,rtorrent,feed,tlsx}/` — shared packages.
- `scripts/make-certs.sh` — openssl cert generator.
- `scripts/migrate-rutorrent.py` — one-shot rutorrent rss-manager → watchlist.yaml.

## getting started

### 1. generate certs

on your local mac:

```
make certs SEEDCTL_HOST=tiny.example.com    # must match what the cluster will dial
```

(this populates `./certs/{ca.crt,server.crt,server.key,client.crt,client.key}` — all gitignored.)

### 2. deploy seedctl on the seed host

```
make deploy-seedhost SEEDCTL_HOST=tiny.example.com
```

idempotent: appends a `@reboot` crontab line that starts seedctl in a tmux session under `$HOME/local/bin/tmux`, and starts the session right away if it isn't already running.

### 3. set up cluster secrets and configmap

```
kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml create namespace torrent-manager

kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager \
  create secret generic tmgr-cookies \
  --from-literal=TRACKER_A_COOKIE='<paste cookie a>' \
  # add more --from-literal flags for each cookie-bearing tracker

kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager \
  create secret generic tmgr-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=client.crt=certs/client.crt \
  --from-file=client.key=certs/client.key

kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager \
  create secret generic tmgr-anthropic \
  --from-literal=ANTHROPIC_API_KEY='<paste key>'

# only needed for tmnotify. use the broker's ip: pods can't resolve .local names.
kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager \
  create secret generic tmgr-mqtt \
  --from-literal=MQTT_URL='tcp://<broker ip>:1883' \
  --from-literal=MQTT_USERNAME='<user>' \
  --from-literal=MQTT_PASSWORD='<password>'
```

edit `deploy/k8s/configmap.yaml` so `seedctl_url`, `roots`, `shows[]`, and `feeds[]` reflect your setup. for feeds that require authentication, add `cookie_env: YOUR_ENV_VAR_NAME` and ensure that key exists in the `tmgr-cookies` secret.

### save directory routing

tmgr routes each matched torrent to a jellyfin-compatible directory on the seed host based on the `category` field in each show rule.

**tv shows** (`category: tv`):

```yaml
roots:
  tv: /home/seedbox/t/tv

shows:
  - name: severance
    regex: '(?i)^Severance.*S\d+E\d+.*1080p'
    category: tv
    directory: severance   # show subdir: lowercase, dot-separated
```

tmgr tells seedctl to put the torrent at `<roots.tv>/<directory>/season<N>` — e.g. `/home/seedbox/t/tv/severance/season2`. season number is extracted from the standard `SxxExx` pattern (case-insensitive, no zero-padding: `season1`, `season10`).

if `directory` is omitted, the show name is normalised: lowercased, non-alphanumeric runs collapsed to `.`, e.g. `Its Always Sunny` → `its.always.sunny`.

**movies** (`category: movies`):

```yaml
roots:
  movies: /home/seedbox/t/movies

shows:
  - name: dune-part-2
    regex: '(?i)^Dune.*Part.Two.*2024.*1080p'
    category: movies
```

torrent lands at `<roots.movies>` directly; rtorrent creates its own subdirectory from the torrent name.

**claude fallback** (rare: release titles without a `SxxExx` marker):

if the regex can't find a season number and `ANTHROPIC_API_KEY` is set, tmgr calls `claude-opus-4-7` to extract `{show, season}` from the raw title. if the key is absent or the call fails, a warning is logged and the torrent is enqueued without a save-directory override (rtorrent uses its configured default).

```
# optional — only needed for unusual release title formats
kubectl -n torrent-manager create secret generic tmgr-anthropic \
  --from-literal=ANTHROPIC_API_KEY='sk-ant-…'
```

if you don't need the ai fallback (most installs won't), skip creating this secret. tmgr will warn at startup but continue normally.

### 4. (optional) migrate from rutorrent

```
mkdir -p private/
scp seedbox@seedbox.example.com:<path>/share/users/<seedbox-user>/settings/rss/cache/info     private/
scp seedbox@seedbox.example.com:<path>/share/users/<seedbox-user>/settings/rss/cache/filters  private/
scripts/migrate-rutorrent.py private/ > /tmp/watchlist.yaml
# review /tmp/watchlist.yaml; passkey-bearing urls are visible — don't paste anywhere public
# each feed entry has a commented-out cookie_env hint — uncomment and set the env var name
# for any tracker that authenticates via a cookie rather than a passkey in the url
# paste the rules into deploy/k8s/configmap.yaml under data.watchlist.yaml
```

`/private/` is gitignored — the raw `info` / `filters` files stay local. `/tmp/watchlist.yaml` is also outside the repo.

### 5. deploy to k3s

```
make image push           # build + push arm64 tmgr image to your registry (override IMAGE / TAG as needed)
make deploy               # kubectl apply -k deploy/k8s/
```

manual smoke test (real network, real cluster):

```
kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager \
  create job --from=cronjob/tmgr tmgr-smoke
kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager logs job/tmgr-smoke -f
```

### completion notifications (tmnotify)

seedctl checks rtorrent every 10s for enqueued torrents that haven't finished, and streams each completion to tmnotify, which publishes it to `MQTT_TOPIC` (default `torrent-manager/complete`) at qos 1, not retained:

```json
{"info_hash": "…", "name": "Some.Show.S01E01.1080p", "title": "…", "save_dir": "tv/some.show/season1", "finished_at": 1760000000}
```

only torrents enqueued by tmgr or tmweb are announced; enqueues that predate the feature are marked as already announced. completions are acked after a successful publish and replayed after any disconnect, so a notification may occasionally arrive twice but won't be lost. run one replica only.

```
make image-import-tmnotify
kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager apply -f deploy/k8s/tmnotify-deployment.yaml
```

to show it on a display, add a home assistant automation with an mqtt trigger on the topic that forwards `name` wherever you like.

### 6. updating the watchlist

```
kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager edit cm tmgr-watchlist
# the next cronjob tick (≤ 5 min) picks up the change.
```

## troubleshooting

- **cronjob 503 from seedctl** → rtorrent isn't reachable on `127.0.0.1:5059`. on the seed host: `tmux attach -t seedctl` and check seedctl's slog output; verify `ss -tlnp | grep 5059` shows rtorrent.
- **cronjob TLS handshake errors** → cert mismatch. confirm the `seedctl_url` hostname matches the server cert's SAN. regenerate with `make certs SEEDCTL_HOST=<correct-name>` and redeploy.
- **no torrents enqueued** → check the regex. trigger a one-off pod (`kubectl create job --from=cronjob/tmgr tmgr-debug`) and watch the slog "match" lines.
- **rtorrent rejected a `.torrent`** → seedctl returns 422. logs include the cert CN, request path, and the rtorrent fault string.

## development

```
make test                 # unit tests
make test-integration     # integration tests (build tag: integration; currently scaffolds, see cmd/{seedctl,tmgr}/integration_test.go)
make lint                 # go vet
make build                # cross-compile both binaries (tmgr arm64 for k3s, seedctl amd64 for seedbox)
make image                # build the tmgr container image
```

## layout

```
cmd/{seedctl,tmgr,tmweb,tmnotify}/  # binary entrypoints
internal/                  # shared packages
  api/                     # http+json wire types
  rtorrent/                # scgi + xmlrpc client
  feed/                    # rss parsing, regex matching, info hash extraction
  tlsx/                    # mtls config loaders + test cert generator
  notify/                  # completion stream subscriber + mqtt publisher
scripts/
  make-certs.sh            # openssl cert generation
  migrate-rutorrent.py     # one-shot rutorrent → watchlist.yaml
deploy/
  k8s/                     # kustomize tree (namespace, configmap, cronjob)
  seedhost/                # idempotent installer + config template
```
