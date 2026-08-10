# Contributing to NimoOS-Common

This repository holds the shared Go library the NimoOS services build on:
JWT verification, logging, HTTP helpers and the service-to-service SDKs.

## Sign your commits

Every commit must carry a `Signed-off-by` line. It certifies that you wrote
the change, or otherwise have the right to submit it under this project's
licence — the [Developer Certificate of Origin](https://developercertificate.org/).

```bash
git commit -s -m "your message"
```

The DCO check runs on every pull request and will not pass without it. If you
forgot on the last commit:

```bash
git commit --amend -s --no-edit && git push --force-with-lease
```

## What CI checks

`go build`, `go vet` and `go test` on every pull request, with `CGO_ENABLED=0`
— this library is pure Go, matching how the services build it.

One package is deliberately excluded: `pkg/mod_management`'s tests are
integration tests that read `/var/run/nimoos/management.url` and call a running
management service, so they can only pass on an installed NimoOS.

## Working on this repository alongside the services

The NimoOS services depend on this module through a `replace` directive
pointing at a sibling checkout, so their `go.mod` version numbers are
decorative. To build a service against a local change here, check both out
side by side:

```
your-workspace/
├── NimoOS-Common/
└── NimoOS-Gateway/      # go.mod: replace github.com/NimoTech/NimoOS-Common => ../NimoOS-Common
```

Changing a shared signature here can break every service at once. If you are
changing an exported API, say so in the pull request.

## Licence

Apache-2.0. By contributing you agree your work is licensed under it. See
[`LICENSE`](./LICENSE) and [`NOTICE`](./NOTICE).
