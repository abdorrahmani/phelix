FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /app .
FROM scratch
COPY --from=build /app /app
USER 65532:65532
ENTRYPOINT ["/app"]
