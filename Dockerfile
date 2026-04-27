FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY main.go go.mod ./
RUN CGO_ENABLED=0 GOOS=linux go build -o hf-bot .

FROM alpine:3.19 AS pydeps

RUN apk add --no-cache python3 py3-pip ca-certificates && \
    python3 -m venv /opt/hfcli && \
    /opt/hfcli/bin/pip install --no-cache-dir huggingface_hub && \
    rm -rf /root/.cache && \
    find /opt/hfcli -type d -name '__pycache__' -prune -exec rm -rf {} + && \
    rm -f /opt/hfcli/bin/pip /opt/hfcli/bin/pip3 /opt/hfcli/bin/pip3.11

FROM alpine:3.19

WORKDIR /app

RUN apk add --no-cache python3 ca-certificates && \
    adduser -D -h /app appuser && \
    mkdir -p /app/data && \
    chown -R appuser:appuser /app /opt

COPY --from=builder /app/hf-bot /app/hf-bot
COPY --from=pydeps /opt/hfcli /opt/hfcli

ENV PATH=/opt/hfcli/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

USER appuser

CMD ["./hf-bot"]
