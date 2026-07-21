# Crucible gateway. Multi-stage build; ~25 MB final.
FROM golang:1.25.12-alpine AS build
WORKDIR /src
# go.mod's replace directives point at ../workers/{sdk-go,stubs/textkit}, which
# live outside this image's build context (compose context: ./gateway) and are
# needed only by gateway/test + textkit e2e — never by ./cmd/gateway. A whole-
# module `go mod download` would try to read those absent paths and fail, so the
# build below fetches exactly the modules the gateway binary imports instead.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates && adduser -D -u 10001 cru
COPY --from=build /out/gateway /usr/local/bin/gateway
USER cru
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
