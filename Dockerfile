# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOWORK=off
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# go.sum is not committed yet: always tidy so every package's checksum is
# recorded (a bare `go mod download` only records go.mod hashes).
RUN go mod tidy
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pixsim ./cmd/pixsim \
 && go build -trimpath -ldflags "-s -w" -o /out/webhook-receiver ./examples/webhook-receiver

# Example consumer used by docker-compose.yml.
FROM gcr.io/distroless/static-debian12:nonroot AS receiver
COPY --from=build /out/webhook-receiver /webhook-receiver
USER nonroot:nonroot
EXPOSE 9000
ENTRYPOINT ["/webhook-receiver"]

FROM gcr.io/distroless/static-debian12:nonroot AS pixsim
COPY --from=build /out/pixsim /pixsim
COPY scenarios /scenarios
USER nonroot:nonroot
ENV PIXSIM_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=3 CMD ["/pixsim", "-healthcheck"]
ENTRYPOINT ["/pixsim"]
