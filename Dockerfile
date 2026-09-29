# syntax=docker/dockerfile:1

FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shortlog-server ./cmd/shortlog-server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 shortlog
COPY --from=build /out/shortlog-server /usr/local/bin/shortlog-server
USER shortlog
ENV APP_ENV=production HTTP_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
# Apply pending migrations first; the server does not start if they fail.
CMD ["sh", "-c", "shortlog-server migrate && exec shortlog-server"]
