FROM golang:1.27-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -o main ./cmd/api

FROM scratch

COPY --from=builder /app/main /main

CMD ["/main"]
