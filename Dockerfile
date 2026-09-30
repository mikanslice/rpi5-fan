FROM golang:1.27-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o fan-controller main.go

FROM scratch

COPY --from=builder /app/fan-controller /app/fan-controller

CMD ["/app/fan-controller"]