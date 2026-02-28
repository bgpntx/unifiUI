FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY go.mod ./
COPY main.go ./
COPY public/ ./public/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o unifiui .

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /build/unifiui /unifiui
EXPOSE 5173
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD ["/unifiui", "healthcheck"]
ENTRYPOINT ["/unifiui"]
