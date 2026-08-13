# images

The Images service. It owns the [image catalog](https://github.com/agynio/architecture/blob/main/architecture/images-service.md):
the `Image` records an organization authors, and the `Version` records the
platform discovers by reading upstream repositories.

It is the only service in the platform that stores a registry URL. The
credential to read that registry is an ordinary Secret the registrar creates,
named here by id and resolved on the internal path. Versions are discovered,
never authored — there is no create, update, or delete API for them.

## Layout

| Path | Contents |
|---|---|
| `cmd/images-service` | Entry point |
| `cmd/provisioner` | Registers the platform's own resources on install and upgrade |
| `internal/store` | `images` and `image_versions` |
| `internal/registry` | Upstream registry reads (`go-containerregistry`) |
| `internal/discovery` | The poll loop and one discovery pass |
| `internal/server` | gRPC surface, authorization, credential handling |
| `internal/provision` | What a release declares must exist, and how it is applied |
| `charts/images` | Helm chart, including the Istio policy guarding the internal RPCs |
| `proto/` | Vendored `agynio/api/images/v1` and `organizations/v1`, temporary — see below |

## Protos

`agynio/api/images/v1` and the `organizations/v1` additions are vendored under
`proto/` so this service builds before the changes land in
`buf.build/agynio/api`. Once it is published there, delete
`proto/` and drop the `directory` input from `buf.gen.yaml`; the module input
already present covers the rest.

```bash
make proto   # buf generate
make test
```

## Local development

Run against the platform VM from sources, per the usual flow:

```bash
devspace dev --namespace platform
```

The sync copies the working tree into the pod but does not restart the process,
so after editing, restart it:

```bash
kubectl exec -n platform deploy/images -- sh -c 'pkill -f "go run"; true'
```

Then run the e2e suite against a port-forward:

```bash
kubectl port-forward -n platform svc/images 15071:50051
```

```bash
IMAGES_ADDRESS=127.0.0.1:15071 go test ./test/e2e/ -tags e2e -count=1 -v
```

The suite registers images against a real public repository, so it exercises
discovery rather than a stub. `TEST_PUBLIC_REPOSITORY` overrides which one.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | required | PostgreSQL connection string |
| `GRPC_ADDRESS` | `:50051` | Listen address |
| `AUTHORIZATION_GRPC_TARGET` | required | Permission checks |
| `SECRETS_GRPC_TARGET` | required | Validating and resolving the Secret an image names |
| `NOTIFICATIONS_GRPC_TARGET` | optional | `image.updated`; without it, Console lists refresh on open |
| `DISCOVERY_INTERVAL` | `15m` | How often each repository is polled |
| `DISCOVERY_TIMEOUT` | `60s` | Budget for one image's pass |

## Provisioning

`cmd/provisioner` registers the platform organization and the images a release
ships. It runs as a Helm hook on install and upgrade; both calls are
create-if-absent, so re-running changes nothing, an operator's edit survives,
and a record they deleted comes back on the next upgrade. What it provisions is
declared in `values.provisioning`, not in the binary.

A failed run does not fail the release — the resource is simply absent until the
next upgrade.

To run it by hand against the VM:

```bash
ORGANIZATIONS_GRPC_TARGET=127.0.0.1:15081 IMAGES_GRPC_TARGET=127.0.0.1:15071 PROVISION_CONFIG=./provision.json go run ./cmd/provisioner
```
