FROM golang:1.24.11-alpine AS builder

WORKDIR /app

RUN apk add git make kubectl


RUN git clone https://github.com/mudler/k8s-mcp-server
RUN cd k8s-mcp-server && go build -o k8s-mcp-server main.go

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

RUN go build -o aish ./

FROM alpine:latest

COPY --from=builder /app/aish /bin/aish
COPY --from=builder /app/k8s-mcp-server/k8s-mcp-server /usr/local/bin/k8s-mcp-server
ENTRYPOINT ["aish"]