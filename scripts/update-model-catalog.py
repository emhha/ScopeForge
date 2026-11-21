#!/usr/bin/env python3
"""Update the source snapshot explicitly; normal builds never download it."""
import datetime
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import urllib.request

SOURCE = "https://models.dev/experimental/v2.0/api.json"
DESTINATION = Path(__file__).resolve().parent.parent / "server" / "modelcatalog"
MAX_BYTES = 32 << 20


def replace_file(path, content):
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as stream:
            temporary = stream.name
            stream.write(content)
        os.replace(temporary, path)
    finally:
        if temporary and os.path.exists(temporary):
            os.unlink(temporary)


def main():
    request = urllib.request.Request(SOURCE, headers={
        "User-Agent": "ScopeForge/model-catalog",
        "Accept": "application/json",
        "Accept-Encoding": "gzip",
    })
    with urllib.request.urlopen(request, timeout=120) as response:
        data = response.read(MAX_BYTES + 1)
        if len(data) > MAX_BYTES:
            raise ValueError("model catalog exceeds 32 MiB")
        if response.headers.get("Content-Encoding") == "gzip":
            with gzip.GzipFile(fileobj=io.BytesIO(data)) as compressed:
                data = compressed.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise ValueError("decoded model catalog exceeds 32 MiB")
    catalog = json.loads(data)
    if not catalog.get("openai", {}).get("models") or not catalog.get("anthropic", {}).get("models"):
        raise ValueError("catalog missing core providers; existing snapshot kept")
    metadata = {
        "source": SOURCE,
        "fetched_at": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z"),
        "sha256": hashlib.sha256(data).hexdigest(),
    }
    replace_file(DESTINATION / "models.dev.v2.json", data)
    replace_file(DESTINATION / "metadata.json", (json.dumps(metadata, indent=2) + "\n").encode())
    print(f"Updated bundled catalog: {len(catalog)} providers, {len(data):,} bytes. Rebuild to include it.")


if __name__ == "__main__":
    main()
