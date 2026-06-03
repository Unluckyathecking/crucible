# Crucible gateway. Multi-stage build; ~25 MB final.
FROM golang:1.25.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates && adduser -D -u 10001 cru
COPY --from=build /out/gateway /usr/local/bin/gateway
USER cru
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
