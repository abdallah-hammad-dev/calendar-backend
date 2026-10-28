# The backend's image: one static binary (no cgo: SQLite is modernc's), zones and CA certificates included.
FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/calendar-backend ./cmd/calendar-backend \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/calendar-backend /calendar-backend
# The database's folder, owned by the image's user so that a new volume mounted there is too.
COPY --from=build --chown=65532:65532 /out/data /data
ENV LISTEN=:8080 DB_PATH=/data/backend.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/calendar-backend"]
CMD ["run"]
