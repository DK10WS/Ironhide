FROM oven/bun:1.4.2-alpine@sha256:d888c0ae6c86d7866ff10c5aafdd9077b36aee6455b33dd270fb93c0dd5cef6f AS build

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile

COPY . .
RUN bun run build

# The Nginx config, including /config.json, comes from the deployment
FROM nginxinc/nginx-unprivileged:1.31.6-alpine-slim@sha256:c81a27f28bc2d9c2da8998444e653c7b85b9bbbaa92e44ef18d8920784e06507

EXPOSE 8080

COPY --from=build /app/dist /usr/share/nginx/html
