FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY main.go go.mod ./
RUN CGO_ENABLED=0 GOOS=linux go build -o hf-bot .

FROM alpine:latest

WORKDIR /app

RUN apk --no-cache add ca-certificates

COPY --from=builder /app/hf-bot .

ENV TELEGRAM_BOT_TOKEN=
ENV HF_TOKEN=
ENV HF_REPO_ID=
ENV CDN_BASE_URL=
ENV HF_FOLDERS=images,videos,documents,others

CMD ["./hf-bot"]