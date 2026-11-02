# Both binaries in one small image. compose.yaml runs each as its own service.
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/cic ./cmd/cic && \
    CGO_ENABLED=0 go build -trimpath -o /out/cic-dockerproxy ./cmd/cic-dockerproxy

FROM scratch
COPY --from=build /out/ /
COPY config/languages.yaml /config/languages.yaml
USER 65532:65532
