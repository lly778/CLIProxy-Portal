FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/portal ./cmd/portal

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata wget \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 portal \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin portal
WORKDIR /app
COPY --from=build /out/portal /usr/local/bin/portal
RUN mkdir -p /data /run/secrets /opt/cliproxy-agy/bin /var/lib/cliproxy-agy && chown -R portal:portal /data /run/secrets /var/lib/cliproxy-agy
USER portal
EXPOSE 18080
ENTRYPOINT ["portal"]
