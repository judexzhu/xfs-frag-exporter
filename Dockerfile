FROM golang:1.23 AS build
WORKDIR /src
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /xfs-frag-exporter .

FROM scratch
COPY --from=build /xfs-frag-exporter /xfs-frag-exporter
USER 65534:65534
EXPOSE 9101
ENTRYPOINT ["/xfs-frag-exporter"]
