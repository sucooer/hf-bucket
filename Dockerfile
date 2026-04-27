FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY main.go go.mod ./
RUN CGO_ENABLED=0 GOOS=linux go build -o hf-bot .

FROM alpine:3.19

WORKDIR /app

RUN apk add --no-cache ca-certificates && \
    adduser -D -h /app appuser && \
    mkdir -p /app/data && \
    chown -R appuser:appuser /app

COPY --from=builder /app/hf-bot /app/hf-bot

USER appuser

CMD ["./hf-bot"]
