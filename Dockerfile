# syntax=docker/dockerfile:1

# Build both MiniCRM and the dummy OIDC identity server from the same source tree.
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/minicrm ./cmd/minicrm \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dev-oidc ./cmd/dev-oidc

# Minimal runtime image. The entrypoint trusts the self-signed dev mail CA (mounted at
# /certs/ca.crt) by pointing Go at a combined CA bundle, then drops to the unprivileged
# "minicrm" user.
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata su-exec \
 && addgroup -S -g 10001 minicrm \
 && adduser -S -D -H -u 10001 -G minicrm minicrm \
 && mkdir -p /data/attachments \
 && chown -R minicrm:minicrm /data

COPY --from=build /out/minicrm /usr/local/bin/minicrm
COPY --from=build /out/dev-oidc /usr/local/bin/dev-oidc
COPY docker/minicrm-entrypoint.sh /usr/local/bin/minicrm-entrypoint

RUN chmod +x /usr/local/bin/minicrm-entrypoint

EXPOSE 8080 9000

ENTRYPOINT ["/usr/local/bin/minicrm-entrypoint"]
CMD ["minicrm"]
