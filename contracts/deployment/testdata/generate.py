#!/usr/bin/env python3
"""Materialize the DATA corpus: one base plus one named mutation per case.

Expected outcomes/rule IDs are reviewed data, not obtained from the validator.
The canonical-byte oracle below is independent of the Go implementation.
"""
import base64
import copy
import hashlib
import json
import pathlib
import shutil

ROOT = pathlib.Path(__file__).resolve().parent


def canonical(value):
    # codefly-json-v1: UTF-8 sorted object keys, ordered arrays, frozen escaping.
    text = json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(',', ':'))
    for char, escape in [('<', r'\u003c'), ('>', r'\u003e'), ('&', r'\u0026'), ('\u2028', r'\u2028'), ('\u2029', r'\u2029')]:
        text = text.replace(char, escape)
    return text.encode('utf-8')


def digest(value):
    return 'sha256:' + hashlib.sha256(value).hexdigest()


def apply_patch(doc, patch):
    parent = doc
    parts = patch['path'].split('/')[1:]
    parts = [p.replace('~1', '/').replace('~0', '~') for p in parts]
    for part in parts[:-1]:
        parent = parent[int(part)] if isinstance(parent, list) else parent[part]
    key = int(parts[-1]) if isinstance(parent, list) and parts[-1] != '-' else parts[-1]
    if patch['op'] == 'copy':
        source = doc
        for part in patch['from'].split('/')[1:]:
            source = source[int(part)] if isinstance(source, list) else source[part]
        if isinstance(parent, list) and key == '-':
            parent.append(copy.deepcopy(source))
        else:
            parent[key] = copy.deepcopy(source)
    elif patch['op'] == 'remove':
        del parent[key]
    elif isinstance(parent, list) and key == '-':
        parent.append(copy.deepcopy(patch['value']))
    else:
        parent[key] = copy.deepcopy(patch['value'])


def generate():
    base = json.loads((ROOT / 'base.json').read_bytes())
    context = json.loads((ROOT / 'base-context.json').read_bytes())
    definitions = json.loads((ROOT / 'mutations.json').read_bytes())
    generated = ROOT / 'generated'
    if generated.exists():
        shutil.rmtree(generated)
    generated.mkdir()
    manifest = {'schema': 'codefly/conformance-corpus/v1', 'cases': []}
    for definition in definitions:
        inventory, ctx = copy.deepcopy(base), copy.deepcopy(context)
        mutation = definition['mutation']
        # A mutation can be an atomic graph change requiring several linked edits.
        for patch in mutation.get('inventory', []):
            apply_patch(inventory, patch)
        for patch in mutation.get('context', []):
            apply_patch(ctx, patch)
        for profile, patches in mutation.get('profiles', {}).items():
            doc = next(d for d in ctx['documents'] if d['id'] == profile)
            content = json.loads(base64.b64decode(doc['content']))
            for patch in patches:
                apply_patch(content, patch)
            retained = canonical(content)
            doc['content'] = base64.b64encode(retained).decode()
            inventory['platform_refs'][profile]['digest'] = digest(retained)
        if 'image' in mutation:
            # Readdress altered evidence: refusal must detect its semantics, not
            # merely a stale digest, unless corruption is the named mutation.
            c = inventory['workloads'][0]['template']['spec']['containers'][0]
            manifest_digest = c['image'].split('@')[1]
            image = json.loads(base64.b64decode(ctx['blobs'][manifest_digest]))
            config_digest = image['config']['digest']
            config = json.loads(base64.b64decode(ctx['blobs'][config_digest]))
            for patch in mutation['image'].get('config', []):
                apply_patch(config, patch)
            config_raw = canonical(config)
            image['config']['digest'] = digest(config_raw)
            image['config']['size'] = len(config_raw)
            ctx['blobs'][digest(config_raw)] = base64.b64encode(config_raw).decode()
            for patch in mutation['image'].get('manifest', []):
                apply_patch(image, patch)
            image_raw = canonical(image)
            ctx['blobs'][digest(image_raw)] = base64.b64encode(image_raw).decode()
            c['image'] = c['image'].split('@')[0] + '@' + digest(image_raw)
        stem = 'generated/' + definition['name']
        input_bytes = json.dumps(inventory, indent=2, ensure_ascii=False).encode() + b'\n'
        if 'raw_replace' in mutation:
            old, new = mutation['raw_replace']
            assert old.encode() in input_bytes
            input_bytes = input_bytes.replace(old.encode(), new.encode(), 1)
        (ROOT / (stem + '.input.json')).write_bytes(input_bytes)
        context_name = 'base-context.json'
        if ctx != context:
            context_name = stem + '.context.json'
            (ROOT / context_name).write_bytes(canonical(ctx) + b'\n')
        expected = None
        expected_digest = None
        if definition['outcome'] == 'valid':
            expected = stem + '.canonical.json'
            expected_bytes = canonical(inventory)
            (ROOT / expected).write_bytes(expected_bytes)
            expected_digest = digest(expected_bytes)
        manifest['cases'].append({
            'name': definition['name'], 'input': stem + '.input.json',
            'context': context_name, 'outcome': definition['outcome'],
            'rule_id': definition['rule_id'], 'canonical': expected,
            'digest': expected_digest,
        })
    (ROOT / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')


if __name__ == '__main__':
    generate()
