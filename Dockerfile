FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /teamachine .

FROM scratch
# idtoken fetches Google's signing certs over HTTPS.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /teamachine /teamachine
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/teamachine"]
