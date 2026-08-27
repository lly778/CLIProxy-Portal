FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/portal ./cmd/portal

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 portal \
    && adduser -S -D -H -u 10001 -G portal portal
WORKDIR /app
COPY --from=build /out/portal /usr/local/bin/portal
RUN mkdir -p /data /run/secrets && chown -R portal:portal /data /run/secrets
USER portal
EXPOSE 18080
ENTRYPOINT ["portal"]
