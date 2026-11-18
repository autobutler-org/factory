# The factory server image: `factory serve` on distroless, as a non-root user.
# Build: make build/image. The Go version matches the go line of go.mod.

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
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
    -ldflags "-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
    -o /out/factory ./cmd/factory

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/factory /factory
USER 65532:65532
ENTRYPOINT ["/factory"]
CMD ["serve"]
