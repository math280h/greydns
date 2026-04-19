# syntax=docker/dockerfile:1.7
FROM golang:1.24 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/controller ./cmd

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/controller /controller

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/controller"]
