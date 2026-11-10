# curator image: static binary on scratch (from configs/docker/Dockerfile.go of the owner's standards). The release
# workflow passes VERSION.

# Build stage runs on the build machine's platform and cross-compiles, so multi-platform images need no emulation.
# golang:alpine follows the latest Go release; go.mod sets the language version.
FROM --platform=$BUILDPLATFORM golang:alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src

# Latest CA bundle for the scratch image (roots missing from an old bundle broke TLS to some services)
RUN apk add --no-cache --upgrade ca-certificates

COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/curator .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# Deployments run "/app/curator healthcheck" as the Docker healthcheck: the binary stays at this path.
COPY --from=build /out/curator /app/curator

# Defaults live in code (README.md, Configuration); compose mounts /data for the state files.
ENTRYPOINT ["/app/curator"]
