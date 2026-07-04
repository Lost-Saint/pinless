# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26.1

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY main.go ./
COPY static ./static
COPY templates ./templates

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    go build -trimpath -ldflags="-s -w" -o /out/pinless .

RUN printf 'nonroot:x:65532:65532:nonroot:/nonexistent:/sbin/nologin\n' > /tmp/passwd \
    && printf 'nonroot:x:65532:\n' > /tmp/group

FROM scratch AS runtime

LABEL org.opencontainers.image.title="pinless" \
      org.opencontainers.image.description="Privacy-focused frontend for browsing public Pinterest content" \
      org.opencontainers.image.licenses="AGPL-3.0-only"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /tmp/passwd /etc/passwd
COPY --from=build /tmp/group /etc/group
COPY --from=build /out/pinless /usr/local/bin/pinless

USER nonroot:nonroot

EXPOSE 3000

ENTRYPOINT ["/usr/local/bin/pinless"]
