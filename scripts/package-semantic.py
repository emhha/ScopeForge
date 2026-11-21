#!/usr/bin/env python3
"""Build a native optional component and a pinned download catalog.

Only release builders need Python, Go and a C compiler. Customers need none of
them. ONNX Runtime is extracted from Microsoft's official wheel; Python itself
is not installed or invoked by the application. --offline adds the model files.
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tempfile
import tarfile
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]
MODEL_REVISION = "614241f622f53c4eeff9890bdc4f31cfecc418b3"
MODEL_ROOT = f"https://huggingface.co/intfloat/multilingual-e5-small/resolve/{MODEL_REVISION}/onnx/"
MODEL_FILES = {
    "model.onnx": ("model_qint8_avx512_vnni.onnx", "dd476dd0c2514e9b9be83aeb3853fac0763e0bdf4a71645407587d77c48a2d88", 118346824),
    "tokenizer.json": ("tokenizer.json", "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39", 17082730),
}
RUNTIME_WHEELS = {
    "linux/amd64": ("manylinux_2_27_x86_64", "1cabe71ca14dcfbf812d312aab0a704507ac909c137ee6e89e4908755d0fc60e"),
    "linux/arm64": ("manylinux_2_27_aarch64", "86bc43e922b1f581b3de26a3dc402149c70e5542fceb5bec6b3a85542dbeb164"),
    "darwin/arm64": ("macosx_14_0_arm64", "79b3119ab9f4f3817062e6dbe7f4a44937de93905e3a31ba34313d18cb49e7be"),
    "windows/amd64": ("win_amd64", "3273c330f5802b64b4103e87b5bbc334c0355fff1b8935d8910b0004ce2f20c8"),
}


def download(url, digest=None):
    with urllib.request.urlopen(url, timeout=120) as response:
        data = response.read()
    if digest and hashlib.sha256(data).hexdigest() != digest:
        raise RuntimeError(f"SHA256 mismatch: {url}")
    return data


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=ROOT / "dist" / "semantic")
    parser.add_argument("--offline", action="store_true", help="also download model and produce an offline zip")
    parser.add_argument("--go", default="go")
    parser.add_argument("--tokenizers-lib-dir", type=Path, help="prebuilt libtokenizers.a matching daulet/tokenizers v1.27.0")
    args = parser.parse_args()
    system = {"Linux": "linux", "Darwin": "darwin", "Windows": "windows"}[platform.system()]
    arch = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
    target = f"{system}/{arch}"
    if target not in RUNTIME_WHEELS:
        parser.error(f"No pinned runtime for {target}; base application still supports keyword retrieval")
    for name, actual in [("GOOS", system), ("GOARCH", arch)]:
        if os.environ.get(name, actual) != actual:
            parser.error("Build the native component on its target platform with a C compiler")
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    worker_name = "worker.exe" if system == "windows" else "worker"
    env = dict(os.environ, CGO_ENABLED="1")
    # sqlite-vec's C binding expects sqlite3.h. Reuse the exact header shipped
    # by the selected mattn amalgamation rather than modifying system packages.
    with tempfile.TemporaryDirectory(prefix="scopeforge-sqlite-header-") as include:
        package = json.loads(subprocess.check_output([args.go, "list", "-m", "-json", "github.com/mattn/go-sqlite3"], cwd=ROOT / "components/semantic-worker", env=env))
        shutil.copyfile(Path(package["Dir"]) / "sqlite3-binding.h", Path(include) / "sqlite3.h")
        native = args.tokenizers_lib_dir
        if native is None:
            if target != "linux/amd64":
                parser.error("This platform needs --tokenizers-lib-dir and release validation; only linux/amd64 has a pinned prebuilt tokenizer")
            native = Path(include)
            data = download("https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.linux-amd64.tar.gz", "72556cdca798dd4ea7cdaba308e5f0d68a8cb93b67c96edf485b7a0edd7b07f4")
            with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
                (native / "libtokenizers.a").write_bytes(archive.extractfile("libtokenizers.a").read())
        env["CGO_LDFLAGS"] = env.get("CGO_LDFLAGS", "") + f" -L{native.resolve()}"
        env["CGO_CFLAGS"] = env.get("CGO_CFLAGS", "") + f" -I{include}"
        subprocess.run([args.go, "build", "-trimpath", "-ldflags=-s -w", "-o", str(out / worker_name), "."], cwd=ROOT / "components/semantic-worker", env=env, check=True)
    runtime_tag, wheel_hash = RUNTIME_WHEELS[target]
    metadata = json.loads(download("https://pypi.org/pypi/onnxruntime/1.24.1/json"))
    wheel = next(f for f in metadata["urls"] if "cp311-cp311" in f["filename"] and runtime_tag in f["filename"])
    runtime_name = {"linux": "libonnxruntime.so", "darwin": "libonnxruntime.dylib", "windows": "onnxruntime.dll"}[system]
    with zipfile.ZipFile(io.BytesIO(download(wheel["url"], wheel_hash))) as z:
        library = next(n for n in z.namelist() if (n.endswith("/onnxruntime.dll") if system == "windows" else "/libonnxruntime." in n and "providers" not in n))
        (out / runtime_name).write_bytes(z.read(library))
        for name in z.namelist():
            if name.endswith(("/LICENSE", "/ThirdPartyNotices.txt")):
                (out / ("ONNX-" + Path(name).name)).write_bytes(z.read(name))
    assets = []
    for name, kind in [(worker_name, "index"), (runtime_name, "runtime")]:
        data = (out / name).read_bytes()
        assets.append(dict(name=name, kind=kind, size=len(data), sha256=hashlib.sha256(data).hexdigest()))
    for name, (source, digest, size) in MODEL_FILES.items():
        url = MODEL_ROOT + source
        assets.append(dict(name=name, kind="model", url=url, size=size, sha256=digest))
        if args.offline:
            existing = out / name
            if not existing.exists() or hashlib.sha256(existing.read_bytes()).hexdigest() != digest:
                existing.write_bytes(download(url, digest))
    manifest = dict(protocol=2, platform=target, version="sqlitevec-onnx-v2", model_version=f"e5-small-{MODEL_REVISION}-qint8-hf127-ort1241-prefix-pool-v1", assets=assets)
    (out / "catalog.json").write_text(json.dumps(manifest, indent=2) + "\n")
    shutil.copyfile(ROOT / "components/semantic-worker/NOTICE.md", out / "NOTICE.md")
    if args.offline:
        archive_path = out.parent / f"semantic-{system}-{arch}.zip"
        with zipfile.ZipFile(archive_path, "w", zipfile.ZIP_DEFLATED) as z:
            for name in ["catalog.json", "NOTICE.md"] + [a["name"] for a in assets] + [p.name for p in out.glob("ONNX-*")]:
                z.write(out / name, name)
        print(archive_path)
    print(f"Component catalog: {out / 'catalog.json'}")


if __name__ == "__main__":
    main()
