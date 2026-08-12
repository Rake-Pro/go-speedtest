# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags "-s -w" -o /out/go-speedtest ./cmd/go-speedtest && \
    go build -trimpath -ldflags "-s -w" -o /out/go-speedtest-cli ./cmd/go-speedtest-cli

# --- runtime stage ---
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/go-speedtest /go-speedtest
COPY --from=build /out/go-speedtest-cli /go-speedtest-cli
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/go-speedtest"]
CMD ["-listen", ":8080", "-mode", "lan"]
