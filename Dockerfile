FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /mailpit-graphapi ./cmd/mailpit-graphapi

FROM scratch
COPY --from=build /mailpit-graphapi /mailpit-graphapi
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/mailpit-graphapi"]
CMD ["--listen", "0.0.0.0:8081", "--mailpit-url", "http://mailpit:8025", "--allow-remote-mailpit"]
