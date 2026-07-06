# syntax=docker/dockerfile:1

FROM golang:1.23 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/burstyrouter ./cmd/burstyrouter

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/burstyrouter /burstyrouter
USER nonroot:nonroot
EXPOSE 8383
ENTRYPOINT ["/burstyrouter"]
