# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Build an embedded Copilot CLI and application binary for the selected target.
RUN go tool bundler --platform "${TARGETOS}/${TARGETARCH}" --output cmd/copilot-api
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath -o /out/copilot-api ./cmd/copilot-api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/copilot-api /usr/local/bin/copilot-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/copilot-api"]
CMD ["serve"]
