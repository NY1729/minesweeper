FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/minesweeper ./cmd/server
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/minesweeper /minesweeper
# The database lives here. Mounted as a volume (see compose.yml), it keeps the game across
# rebuilds; a new volume starts out owned by this (non-root) user because of the chown below.
COPY --from=build --chown=nonroot:nonroot /data /data
ENV DATABASE_PATH=/data/minesweeper.db
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/minesweeper"]
