# torrent-manager

automated rss → rtorrent pipeline. polls feeds from a private tracker on a 5-minute cadence in a k3s CronJob, enqueues matching torrents into a remote rtorrent over mtls.

design: see [[design-spec]] in obsidian.
plan: see [[impl-plan]] in obsidian.

## quick start

```
make certs
make build
```

(deploy instructions land here once tasks 12–15 are complete.)
