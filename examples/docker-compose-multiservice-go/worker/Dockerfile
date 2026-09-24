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
