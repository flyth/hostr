# Both build stages pin themselves to the *build* platform and cross-compile,
# so a linux/amd64 + linux/arm64 manifest costs two Go links rather than an
# emulated npm install under QEMU.

# 1. Build the control panel. Its output is just files — no architecture.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# 2. Build the static binaries with the panel embedded.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
# mailcap supplies /etc/mime.types; Go's built-in table misses .woff2, .ico and
# friends, which a static site will absolutely serve.
RUN apk add --no-cache mailcap
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/build/ ./web/
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/hostr . \
 && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/hostrctl ./cmd/hostrctl
# Pre-create the data directory so the unprivileged runtime user owns it.
RUN mkdir -p /out/data && chown 65534:65534 /out/data

# 3. Ship it. Scratch is enough: no cgo, no shell, nothing to exploit.
FROM scratch
COPY --from=build /out/hostr /hostr
COPY --from=build /out/hostrctl /hostrctl
COPY --from=build /etc/mime.types /etc/mime.types
COPY --from=build --chown=65534:65534 /out/data /data

USER 65534:65534
VOLUME /data
EXPOSE 8080
ENV HOSTR_DATA=/data HOSTR_ADDR=:8080
ENTRYPOINT ["/hostr"]
