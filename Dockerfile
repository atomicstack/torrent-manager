# build stage — cgo-enabled because internal/hashcache + internal/itemsdb use
# mattn/go-sqlite3. apk pulls in gcc + musl headers + sqlite headers; the
# resulting binary is musl-linked, so the runtime stage uses alpine (not
# scratch, which has no libc).
FROM golang:1.26-alpine AS build
RUN apk add --no-cache build-base sqlite-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETARCH=arm64
RUN CGO_ENABLED=1 GOOS=linux GOARCH=$TARGETARCH \
    go build -tags=sqlite_fts5 -ldflags="-s -w" -o /out/tmgr ./cmd/tmgr

# runtime stage — alpine minimal; ca-certificates for outbound https to feeds
# and seedctl, tzdata so slog timestamps have a sane local tz if the cron pod
# is ever inspected interactively, sqlite-libs for the musl-linked sqlite3 calls.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata sqlite-libs
COPY --from=build /out/tmgr /tmgr
USER 65532:65532
ENTRYPOINT ["/tmgr"]
