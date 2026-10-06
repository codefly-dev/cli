#!/usr/bin/env python3
"""Run the SAME data corpus via the public Python executable adapter (A27)."""
import json
import pathlib
import sys

from deployment_contract import ValidationError, validate

root = pathlib.Path(__file__).resolve().parents[1] / 'testdata'
manifest = json.loads((root / 'manifest.json').read_bytes())
for case in manifest['cases']:
    try:
        result = validate(sys.argv[1], (root / case['input']).read_bytes(), (root / case['context']).read_bytes())
    except ValidationError as error:
        assert case['outcome'] == 'invalid', (case['name'], error)
        assert error.violation['rule'] == case['rule_id'], (case['name'], error)
    else:
        assert case['outcome'] == 'valid', case['name']
        assert result['canonical'] == (root / case['canonical']).read_bytes(), case['name']
        assert result['digest'] == case['digest'], case['name']
        for row in result['rows']:
            assert set(row) == {'ns', 'labels', 'sa', 'container', 'images', 'app', 'init'}
print(f"Python/Go executable conformance: {len(manifest['cases'])} cases passed")
