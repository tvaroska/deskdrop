# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build
ARG VERSION=v1.4.3
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/deskdrop.exe ./cmd/deskdrop && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/deskdrop ./cmd/deskdrop && \
    (cd /out && sha256sum deskdrop deskdrop.exe > SHA256SUMS.txt)

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/server /server
COPY --from=build /out/deskdrop.exe /release/deskdrop.exe
COPY --from=build /out/deskdrop /release/deskdrop
COPY --from=build /out/SHA256SUMS.txt /release/SHA256SUMS.txt
USER 65532:65532
EXPOSE 8080
ENV LISTEN_ADDR=:8080
ENTRYPOINT ["/server"]
