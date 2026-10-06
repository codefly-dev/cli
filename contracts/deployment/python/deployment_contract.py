"""Transport adapter for the pinned deployment-contract executable.

This is not a Python validator. The caller authenticates the validation context
independently and supplies the exact released executable path (never PATH lookup).
Successful validation is not platform authorization.
"""
import json
import pathlib
import subprocess


class ValidationError(ValueError):
    def __init__(self, violation):
        self.violation = violation
        super().__init__(f"{violation['rule']} at {violation['path']}: {violation['message']}")


def validate(executable, inventory_bytes, context_bytes):
    """Return canonical bytes, digest and seven-key rows from the Go executable.

    Raw documents are preserved so Go detects duplicate keys and invalid Unicode.
    The caller owns release pinning and authentication of the executable/context.
    """
    executable = pathlib.Path(executable)
    if not executable.is_absolute():
        raise ValueError("the pinned executable path must be absolute")
    request = b'{"inventory":' + inventory_bytes + b',"context":' + context_bytes + b'}'
    completed = subprocess.run(
        [str(executable), "validate"], input=request, capture_output=True,
        check=False, timeout=30,
    )
    if completed.returncode not in (0, 1):
        raise RuntimeError("deployment-contract executable failed")
    response = json.loads(completed.stdout)
    if completed.returncode == 1:
        if response.get("valid") is not False or response.get("violation") is None:
            raise RuntimeError("invalid deployment-contract refusal response")
        raise ValidationError(response["violation"])
    if response.get("valid") is not True or response.get("violation") is not None:
        raise RuntimeError("invalid deployment-contract success response")
    return {
        "canonical": response["canonical"].encode("utf-8"),
        "digest": response["digest"],
        "rows": response["rows"],
    }
