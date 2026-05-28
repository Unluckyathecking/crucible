# Crucible worker stub (TypeScript/Node.js).
# Build context MUST be the repo root so the sdk-ts package resolves.
#   docker build -f workers/stubs/ts/Dockerfile .
FROM node:22-alpine AS build
WORKDIR /src
COPY workers/sdk-ts ./workers/sdk-ts
COPY workers/stubs/ts ./workers/stubs/ts
RUN cd workers/sdk-ts && npm ci && npm run build
RUN cd workers/stubs/ts && npm ci && npm run build

FROM node:22-alpine
RUN adduser -D -u 10001 cru
WORKDIR /app
COPY --from=build /src/workers/stubs/ts/dist ./dist
COPY --from=build /src/workers/sdk-ts/package.json ./node_modules/@crucible/worker-sdk-ts/package.json
COPY --from=build /src/workers/sdk-ts/dist ./node_modules/@crucible/worker-sdk-ts/dist
USER cru
EXPOSE 8081
CMD ["node", "dist/index.js"]
