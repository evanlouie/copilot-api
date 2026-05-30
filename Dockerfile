# syntax=docker/dockerfile:1.7

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build a Linux-matched embedded Copilot CLI for the container target.
RUN go tool bundler --platform linux/amd64 --output cmd/copilot-api
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/copilot-api ./cmd/copilot-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/copilot-api /usr/local/bin/copilot-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/copilot-api"]
CMD ["serve"]
