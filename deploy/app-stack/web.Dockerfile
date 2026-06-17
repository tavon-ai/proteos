# syntax=docker/dockerfile:1
#
# Web image: build the React SPA, serve it with nginx, and reverse-proxy /api to
# the control plane. The SPA calls the API with relative paths (/api/...), and
# the session-cookie + CSRF model depends on the SPA and API sharing ONE origin,
# so nginx fronting both is the correct production shape — not Vite's dev proxy.
#
# Build context MUST be the repo root (needs web/ and deploy/app-stack/nginx.conf).

FROM node:25-alpine AS build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM nginx:1.27-alpine
COPY deploy/app-stack/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /web/dist /usr/share/nginx/html
EXPOSE 80
