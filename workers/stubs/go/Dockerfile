# Crucible worker stub.
# Build context MUST be the repo root so the sdk-go replace directive resolves.
#   docker build -f workers/stubs/go/Dockerfile .
FROM golang:1.25.0-alpine AS build
WORKDIR /src
COPY workers/sdk-go ./workers/sdk-go
COPY workers/stubs/go ./workers/stubs/go
WORKDIR /src/workers/stubs/go
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/worker .

FROM alpine:3.20
RUN apk add --no-cache wget && adduser -D -u 10001 cru
COPY --from=build /out/worker /usr/local/bin/worker
USER cru
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/worker"]
