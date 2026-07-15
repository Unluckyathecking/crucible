# Crucible gateway. Multi-stage build; ~25 MB final.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY gateway/go.mod gateway/go.sum ./gateway/
COPY workers/sdk-go/go.mod workers/sdk-go/go.sum ./workers/sdk-go/
COPY workers/stubs/textkit/go.mod workers/stubs/textkit/go.sum ./workers/stubs/textkit/
WORKDIR /src/gateway
RUN go mod download
COPY gateway ./
COPY workers/sdk-go ../workers/sdk-go
COPY workers/stubs/textkit ../workers/stubs/textkit
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates && adduser -D -u 10001 cru
COPY --from=build /out/gateway /usr/local/bin/gateway
USER cru
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
