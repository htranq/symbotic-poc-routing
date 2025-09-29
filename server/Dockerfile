FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/server ./main.go

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=builder /out/server /app/server
ENV PORT=8081
EXPOSE 8081
ENTRYPOINT ["/app/server"]


