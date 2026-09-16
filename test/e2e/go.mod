// The E2E suite is a module of its own: it is never compiled into the
// image (the Dockerfile's gobuild stage only COPYs entrypoint/), it must
// not add dependencies to the entrypoint module, and it is driven by
// `go test` from the repository root or from CI.
module github.com/esitc-paris/samba-ad-dc/test/e2e

go 1.25.0
