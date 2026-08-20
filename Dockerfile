# syntax=docker/dockerfile:1

# --- build ---------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first so the layer is cached across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The store uses modernc/sqlite (pure Go), so the binary is fully static.
# Keep the //go:debug tlssha1=1 directive in main.go working: it is compiled
# into the binary, no build flag needed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/olx-notifier .

# --- runtime -------------------------------------------------------------
FROM alpine:3.22

# HTTPS to OLX and the Matrix homeserver needs root certs; tzdata keeps
# timestamps sane if TZ is set.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 -h /data olx

COPY --from=build /out/olx-notifier /usr/local/bin/olx-notifier

# /data is the host-mounted directory: config.yaml in, olx.db out.
VOLUME /data
WORKDIR /data
USER olx

ENTRYPOINT ["/usr/local/bin/olx-notifier"]
CMD ["-config", "/data/config.yaml"]
