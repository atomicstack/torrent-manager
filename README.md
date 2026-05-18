# torrent-manager

automated rss → rtorrent pipeline. a k3s CronJob polls a private tracker's rss feeds every 5 minutes, matches items against a regex watchlist, and posts matching `.torrent` files to a tiny mtls-protected http server running alongside rtorrent on a remote seed host.

- design: see `[[design-spec]]` in the obsidian agent-notes vault (`torrent-manager/2026-05-18/design-spec`)
- plan: see `[[impl-plan]]` in the obsidian agent-notes vault (`torrent-manager/2026-05-18/impl-plan`)

## components

- `cmd/seedctl/` — long-lived mtls http server on the seed host. proxies a narrow API (`POST /v1/torrents`, `GET /v1/torrents/{hash}`, `GET /healthz`) into rtorrent's xmlrpc-over-scgi.
- `cmd/tmgr/` — one-shot CronJob binary. polls feeds, matches, dedups via seedctl, enqueues new items.
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
  create secret generic tmgr-tracker \
  --from-literal=TRACKER_COOKIE='<paste cookie here>'

kubectl --kubeconfig ~/.kube/rpi-sandbox.yaml -n torrent-manager \
  create secret generic tmgr-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=client.crt=certs/client.crt \
  --from-file=client.key=certs/client.key
```

edit `deploy/k8s/configmap.yaml` so `seedctl_url`, `shows[]`, and `feeds[]` reflect your tracker.

### 4. (optional) migrate from rutorrent

```
mkdir -p private/
scp seedbox@tiny:<path>/share/users/<seedbox-user>/settings/rss/cache/info     private/
scp seedbox@tiny:<path>/share/users/<seedbox-user>/settings/rss/cache/filters  private/
scripts/migrate-rutorrent.py private/ > /tmp/watchlist.yaml
# review /tmp/watchlist.yaml; passkey-bearing urls are visible — don't paste anywhere public
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
cmd/{seedctl,tmgr}/        # binary entrypoints
internal/                  # shared packages
  api/                     # http+json wire types
  rtorrent/                # scgi + xmlrpc client
  feed/                    # rss parsing, regex matching, info hash extraction
  tlsx/                    # mtls config loaders + test cert generator
scripts/
  make-certs.sh            # openssl cert generation
  migrate-rutorrent.py     # one-shot rutorrent → watchlist.yaml
deploy/
  k8s/                     # kustomize tree (namespace, configmap, cronjob)
  seedhost/                # idempotent installer + config template
```
