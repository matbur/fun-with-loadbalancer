ARG GO_VERSION=1.27.0

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local

ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/lb ./cmd/lb

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="fun-with-loadbalancer" \
      org.opencontainers.image.description="HTTP load balancer with Prometheus metrics and an admin panel"

COPY --from=build /out/lb /lb

USER nonroot:nonroot

ENTRYPOINT ["/lb"]
