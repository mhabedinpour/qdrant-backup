# Stage 1: Build
FROM golang:1.22.1 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix nocgo -o backup main.go

FROM debian:buster-slim
WORKDIR /app

RUN apt-get update && \
    apt-get install -y ca-certificates curl net-tools && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/backup /app/backup

ENTRYPOINT [ "/app/backup" ]
CMD [ ]
