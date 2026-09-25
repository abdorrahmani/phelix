# Multi-stage build with a named `prod` target. docker-compose's `app` service
# points its build.target here, and Phelix (deploy.docker.build: compose) builds
# through that same compose service — so the build config lives in ONE place
# (compose) and is never duplicated into a standalone Phelix Dockerfile.
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/app .

# The runtime target compose selects with build.target: prod. No EXPOSE /
# hardcoded port — the app binds $PORT.
FROM alpine:3.20 AS prod
RUN adduser -D -u 10001 app
USER app
COPY --from=build /bin/app /bin/app
ENTRYPOINT ["/bin/app"]
