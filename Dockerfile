FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/ggrocco/icaro/internal/cli.version=${VERSION}" -o /out/icaro ./cmd/icaro

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/icaro /icaro
VOLUME ["/data"]
ENV ICARO_DATA_DIR=/data
EXPOSE 8787
ENTRYPOINT ["/icaro"]
CMD ["serve", "--role", "all"]
