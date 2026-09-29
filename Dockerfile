FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /teamachine .

FROM scratch
COPY --from=builder /teamachine /teamachine
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/teamachine"]
