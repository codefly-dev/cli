# Coordinate fixtures

`infra-base-lodestar.json` is byte-for-byte output of infra-base commit
`fb376be7c7d2c0d85677d164b85399774ecbbeb6`:

```
env -u OBIN_REGISTRY -u OBIN_REPO_ROOT bin/obinctl coordinate-contract hosted-us-east1 --namespace lodestar
```

It carries public configuration, Accounts' exact primary workload identity,
ExternalSecret references and transformation, cluster context and reviewed
delivery source. It contains no credentials and no managed-services declaration.
Import/render tests verify the effective Kustomize output, including rejection
of a redirected secret template. They do not prove live cloud authentication.
Other fixtures exercise the CLI contract, not an infrastructure producer.
