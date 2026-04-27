FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY main.go go.mod ./
RUN CGO_ENABLED=0 GOOS=linux go build -o hf-bot .

FROM alpine:3.19

WORKDIR /app

RUN apk add --no-cache python3 py3-pip ca-certificates curl && \
    pip3 install --no-cache-dir --break-system-packages huggingface_hub packaging && \
    apk del py3-pip

COPY --from=builder /app/hf-bot .

ENV TELEGRAM_BOT_TOKEN=
ENV HF_TOKEN=
ENV CDN_BASE_URL=
ENV HF_FOLDERS=images,videos,documents,others

CMD ["./hf-bot"]