# Example docker image with Ztunnel for Lambda
FROM golang:1.23.0-alpine AS base
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

FROM base AS build
RUN --mount=target=. \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -o /out/bin -trimpath .

# hadolint ignore=DL3026
FROM public.ecr.aws/lambda/provided:al2023

COPY --from=build /out/bin /usr/bin/echo
# BEFORE:
# ENTRYPOINT ["/usr/bin/echo"]

# With Ztunnel: add the COPY, and modify the ENTRYPOINT
# Replace with the real Ztunnel image
# note: currently we are not publishing musl (static) builds, so the base image MUST have glibc installed
# hadolint ignore=DL3026
COPY --from=120424078857.dkr.ecr.us-west-2.amazonaws.com/howardjohn:ztunnel-ecs-musl-1724371493 /usr/local/bin/ztunnel /usr/local/bin/ztunnel
ENTRYPOINT ["/usr/local/bin/ztunnel", "lambda", "/usr/bin/echo"]
