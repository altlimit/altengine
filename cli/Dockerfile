# The altengine local emulator as a small image for docker compose:
#
#   services:
#     altengine:
#       image: ghcr.io/altlimit/altengine
#       ports: ["127.0.0.1:9191:9191"]
#       volumes: ["altengine:/data"]
#
# Other services reach it at http://altengine:9191. That works because ALTENGINE_ALLOW_HOST
# defaults to "altengine". If the service has another name, set ALTENGINE_ALLOW_HOST to it.
#
# A static binary on scratch, with nothing else in the image. Containers are the one service it
# lacks, because there is no docker CLI or daemon inside, so container jobs answer 503.
#
# Build from cli/:  docker build -t altengine .

# Cross-compile on the build machine's own architecture. Go needs no emulation for another
# GOARCH, so a multi-arch build costs one native compile per platform.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/altengine ./cmd/altengine \
 && mkdir -p /out/data

FROM scratch
# CA roots for the commands that reach the hosted service, and for --deploy from an https URL.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/altengine /altengine
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV ALTENGINE_ALLOW_HOST=altengine
VOLUME /data
# 9191 is the API and admin console. 9192 is the site served with --static.
EXPOSE 9191 9192
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s CMD ["/altengine", "health"]
ENTRYPOINT ["/altengine"]
# 0.0.0.0 is the container's own interface. The emulator is unauthenticated, so publish the port
# on the host's loopback (127.0.0.1:9191:9191), not on every interface.
CMD ["dev", "--host", "0.0.0.0", "--data", "/data"]
