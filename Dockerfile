# syntax=docker/dockerfile:1.8

FROM golang:1.25 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/images ./cmd/images-service

# Discovery makes outbound HTTPS calls to whatever registries organizations have
# registered, so the runtime needs a CA bundle. base-debian12 carries one.
FROM gcr.io/distroless/base-debian12 AS runtime

WORKDIR /app

COPY --from=builder /out/images /usr/local/bin/images

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/images"]
