# Build context: repository root (pnpm workspace).
# Build stage
FROM node:22-alpine AS builder

WORKDIR /app

RUN corepack enable && corepack prepare pnpm@10.31.0 --activate

# Workspace manifests first for dependency-layer caching
COPY pnpm-workspace.yaml package.json pnpm-lock.yaml ./
COPY apps/web/package.json apps/web/package.json

RUN pnpm install --frozen-lockfile --filter @vtx/web

COPY apps/web apps/web

ENV NEXT_TELEMETRY_DISABLED=1
# The API proxy target is read at request time, so one image serves the
# Cloudflare Tunnel and a LAN address without a rebuild.
ENV INTERNAL_API_URL=http://backend:49080
RUN pnpm --filter @vtx/web build

# Runtime stage
FROM node:22-alpine AS runner

WORKDIR /app

RUN corepack enable && corepack prepare pnpm@10.31.0 --activate

ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1

COPY --from=builder /app/package.json /app/pnpm-workspace.yaml ./
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/apps/web ./apps/web

WORKDIR /app/apps/web

EXPOSE 49030

CMD ["pnpm", "start"]
