FROM golang:1.21 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mini-coin ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mini-coin /mini-coin
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/mini-coin"]
