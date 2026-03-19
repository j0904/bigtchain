# syntax=docker/dockerfile:1
# Multi-stage build for BIGT chain validator node.
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /bigt ./cmd/bigt
RUN CGO_ENABLED=1 go build -o /validator ./cmd/validator

# Runtime image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates jq curl bash

COPY --from=builder /bigt /usr/local/bin/bigt
COPY --from=builder /validator /usr/local/bin/validator

EXPOSE 26656 26657 26660

VOLUME ["/root/.bigt"]

ENTRYPOINT ["bigt"]
CMD ["--home", "/root/.bigt"]
