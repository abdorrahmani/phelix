# Multi-stage build → static binary in a minimal runtime. Used by BOTH
# `docker compose --profile local up` (local dev) and Phelix's default
# "dockerfile" build strategy on the server. No EXPOSE / hardcoded port — the app
# binds $PORT, which Phelix (or compose) injects.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/app .

FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=build /bin/app /bin/app
ENTRYPOINT ["/bin/app"]
