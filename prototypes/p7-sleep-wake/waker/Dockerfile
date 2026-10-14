FROM golang:1.23.4-alpine AS build
WORKDIR /src
COPY main.go .
RUN go mod init waker >/dev/null 2>&1 && CGO_ENABLED=0 go build -o /waker .

# The real agent runs on the host; here it is a container with the Docker CLI and socket.
FROM docker:27.3.1-cli
COPY --from=build /waker /usr/local/bin/waker
ENTRYPOINT ["waker"]
