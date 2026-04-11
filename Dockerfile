FROM golang:1.20 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/mcp-tools ./cmd/mcp-tools

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/mcp-tools /usr/local/bin/mcp-tools

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/mcp-tools"]
