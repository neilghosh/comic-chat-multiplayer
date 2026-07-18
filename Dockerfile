FROM golang:1.26.5-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o comic-chat-server .

FROM alpine:3.22
RUN apk --no-cache add ca-certificates
WORKDIR /app

COPY --from=builder /app/comic-chat-server .
COPY --from=builder /app/static ./static

ENV PORT=8080
ENV GENERATED_DIR=/app/static/generated

EXPOSE 8080
CMD ["./comic-chat-server"]
