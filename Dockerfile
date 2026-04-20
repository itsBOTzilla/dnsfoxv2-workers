FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /workers ./main.go

FROM alpine:3.23
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /workers .
CMD ["./workers"]
