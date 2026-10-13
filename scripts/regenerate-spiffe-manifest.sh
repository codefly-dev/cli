#!/usr/bin/env bash
# Reproduce the reviewed v0.15.0 chart render, then pin every image.
set -euo pipefail
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
chart=cert-manager-csi-driver-spiffe-v0.15.0.tgz
curl --fail --location --proto '=https' --tlsv1.2 \
  "https://charts.jetstack.io/charts/$chart" -o "$work_dir/$chart"
python3 - "$work_dir/$chart" <<'PY'
import hashlib, pathlib, sys
actual = hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest()
expected = 'd92b09dc742f3fd8d2a8caa161a5532e6202f1455e31e1ca34670a98bee7b650'
if actual != expected:
    sys.exit(f'chart SHA-256 {actual} differs from reviewed {expected}')
PY
helm template csi-driver-spiffe "$work_dir/$chart" --namespace cert-manager \
  -f "$repo_dir/pkg/deployments/manifests/csi-values.yaml" > "$work_dir/rendered.yaml"
python3 - "$repo_dir" "$work_dir/rendered.yaml" <<'PY'
import hashlib, pathlib, re, sys
root = pathlib.Path(sys.argv[1]) / 'pkg/deployments/manifests'
pins = dict(line.split() for line in (root / 'csi-images.txt').read_text().splitlines())
raw = pathlib.Path(sys.argv[2]).read_text()
pattern = r'''(?m)^(\s*(?:-\s+)?image:\s*)["']?([^\s"']+)["']?\s*$'''
seen = set()
def pin(match):
    reference = match[2]
    if reference not in pins:
        sys.exit(f'unpinned image: {reference}')
    seen.add(reference)
    return match[1] + reference + '@' + pins[reference]
raw = re.sub(pattern, pin, raw)
if seen != set(pins):
    sys.exit('rendered image set differs from reviewed image set')
(root / 'csi-driver-spiffe.yaml').write_text(raw)
print('spiffeManifestSHA256 = "' + hashlib.sha256(raw.encode()).hexdigest() + '"')
print('Review the diff and update the digest in pkg/deployments/local_spiffe.go.')
PY
