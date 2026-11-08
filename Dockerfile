# Cross-compiles on the build host, so multi-platform builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /stillproof-anchor ./cmd/stillproof-anchor
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /stillproof-anchor /stillproof-anchor
COPY --from=build --chown=nonroot:nonroot /data /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/stillproof-anchor", "-listen", ":8080", "-db", "/data/stillproof.db"]
