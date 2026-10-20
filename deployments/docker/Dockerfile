# syntax=docker/dockerfile:1
# One image, three entry points: dist/api.js, dist/worker.js, dist/admin.js.
FROM node:22-alpine AS build
WORKDIR /src
RUN corepack enable
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml tsconfig.base.json ./
COPY packages/shared/package.json packages/shared/
COPY apps/server/package.json apps/server/
COPY apps/gateway/package.json apps/gateway/
RUN --mount=type=cache,id=pnpm,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile --filter "@smsai/server..."
COPY packages/shared/ packages/shared/
COPY apps/server/ apps/server/
RUN pnpm --filter @smsai/server build

# The bundles are self-contained: no node_modules at runtime.
FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build /src/apps/server/dist/ /app/dist/
COPY apps/server/public/ /app/public/
COPY data/ /app/data/
COPY migrations/ /app/migrations/
USER node
EXPOSE 8080
CMD ["node", "--enable-source-maps", "dist/api.js"]
