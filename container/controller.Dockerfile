# Build the controller binary and ship it on a minimal, non-root base.
#
# Multi-arch: the build stage runs on the *builder's* platform (no QEMU) and cross-compiles
# for TARGETOS/TARGETARCH, so `--platform linux/amd64,linux/arm64` costs nothing extra.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/gitea-k8s-runner ./cmd/gitea-k8s-runner

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gitea-k8s-runner /usr/local/bin/gitea-k8s-runner
ENTRYPOINT ["/usr/local/bin/gitea-k8s-runner"]
CMD ["controller"]
