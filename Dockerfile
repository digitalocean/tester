FROM golang:1.26 AS build
COPY . /tester
WORKDIR /tester
RUN make build

# The runtime image stays a golang image on purpose: `tester run` shells out to
# `go tool test2json` to parse test binary output, and downstream images
# (digitalocean/e2e) build their test binaries in their own stage and copy them
# in on top of this one.
FROM golang:1.26
COPY --from=build /tester/dist/tester-linux-amd64 /bin/tester
