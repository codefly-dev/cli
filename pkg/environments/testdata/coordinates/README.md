# Coordinate fixtures

`infra-base-lodestar.json` is byte-for-byte output of infra-base commit
`7011c60b99116c397e73a8ad11e9cf5afe6f2044`:

```
uv run --locked --directory tools/obin-cli obinctl coordinate-contract --all --out-dir <output>
```

The output file is `hosted-us-east1.lodestar.json`. It carries public identifiers,
not credentials. It does not yet declare workload identity or secret delivery.
Import/render tests prove transport, not application support for these keys or
successful cloud authentication. Other fixtures exercise the CLI contract and
are not claims about current producer output.
