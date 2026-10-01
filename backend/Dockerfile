FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/minesweeper ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/minesweeper /minesweeper
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/minesweeper"]
