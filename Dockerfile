# The factory server image: `factory serve` on distroless, as a non-root user,
# with the web UI embedded. Build: make build/image. The Go version matches the
# go line of go.mod; the Node version matches web/.nvmrc.

# The web UI, built on the build platform: its output is the same for every
# architecture.
FROM --platform=$BUILDPLATFORM node:26.13.1-trixie-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
# Writes /src/internal/server/public, which the Go build embeds.
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/server/public ./internal/server/public
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags "-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
    -o /out/factory ./cmd/factory

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/factory /factory
USER 65532:65532
ENTRYPOINT ["/factory"]
CMD ["serve"]
