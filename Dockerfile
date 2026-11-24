# Static, CGO-free binary on a distroless base. Data lives in /data (mount a volume there).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/bookroom ./cmd/bookroom \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bookroom /usr/local/bin/bookroom
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV BOOKROOM_DATA_DIR=/data \
    BOOKROOM_ADDR=:8080
VOLUME ["/data"]
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/bookroom"]
CMD ["serve"]
