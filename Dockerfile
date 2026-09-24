# Cross-compiling from the build platform, so a multi-arch build needs no
# emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETARCH
ARG version=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-X github.com/Ruakij/csi-rclone/pkg/rclone.DriverVersion=${version}" \
    -o /out/csi-rclone-plugin ./cmd/csi-rclone-plugin

FROM rclone/rclone:1.74.3@sha256:623378ad0ff3ebd5cebf77720843c0e02edfe46e2d5b5ac6bed54c6371780dfb AS rclone

FROM alpine:3.23
RUN apk add --no-cache ca-certificates bash fuse3 tini

COPY --from=rclone /usr/local/bin/rclone /usr/bin/rclone
COPY --from=build /out/csi-rclone-plugin /bin/csi-rclone-plugin

ENTRYPOINT [ "/sbin/tini", "-s", "--"]
CMD ["/bin/csi-rclone-plugin"]
