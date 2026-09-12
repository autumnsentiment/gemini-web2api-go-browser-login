#!/usr/bin/env python3
"""Package a Chromium extension directory into a CRX3 and emit its extension ID.

用法:
  pack_crx.py <ext_dir> <out_crx> [--key <pem>]

CRX3 结构（Chromium 官方格式）:
  "Cr24" | uint32 version=3 | uint32 header_len | CrxFileHeader(protobuf) | zip
签名覆盖:
  b"CRX3 SignedData\\x00" | uint32le(len(signed_header_data)) | signed_header_data | zip
其中 signed_header_data = SignedData{ crx_id = sha256(pubkey_der)[:16] }

只依赖标准库 + 系统 openssl（rsa-sha256 签名）。
"""
import hashlib
import io
import os
import struct
import subprocess
import sys
import tempfile
import zipfile


def varint(n):
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def tag(field, wire):
    return varint((field << 3) | wire)


def pb_bytes(field, data):
    return tag(field, 2) + varint(len(data)) + data


def make_zip(ext_dir):
    buf = io.BytesIO()
    # 固定时间戳，保证可复现
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        for root, dirs, files in os.walk(ext_dir):
            dirs.sort()
            for fn in sorted(files):
                full = os.path.join(root, fn)
                rel = os.path.relpath(full, ext_dir).replace(os.sep, "/")
                zi = zipfile.ZipInfo(rel, date_time=(2024, 1, 1, 0, 0, 0))
                zi.compress_type = zipfile.ZIP_DEFLATED
                zi.external_attr = 0o644 << 16
                with open(full, "rb") as f:
                    z.writestr(zi, f.read())
    return buf.getvalue()


def ext_id_from_pubkey(pub_der):
    h = hashlib.sha256(pub_der).hexdigest()[:32]
    return "".join(chr(ord("a") + int(c, 16)) for c in h)


def ensure_key(path):
    if os.path.exists(path):
        return
    subprocess.check_call(["openssl", "genrsa", "-out", path, "2048"],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def sign(msg, key_path):
    with tempfile.NamedTemporaryFile(delete=False) as f:
        f.write(msg)
        tmp = f.name
    try:
        p = subprocess.run(["openssl", "dgst", "-sha256", "-sign", key_path, tmp],
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)
        return p.stdout
    finally:
        os.unlink(tmp)


def pubkey_der(key_path):
    p = subprocess.run(["openssl", "rsa", "-in", key_path, "-pubout", "-outform", "DER"],
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)
    return p.stdout


def main():
    ext_dir, out_crx = sys.argv[1], sys.argv[2]
    key = None
    if "--key" in sys.argv:
        key = sys.argv[sys.argv.index("--key") + 1]
    key = key or os.path.join(os.path.dirname(os.path.abspath(out_crx)), "gw2a-ext.pem")
    ensure_key(key)

    zip_bytes = make_zip(ext_dir)
    pub = pubkey_der(key)
    crx_id = hashlib.sha256(pub).digest()[:16]
    signed_header_data = pb_bytes(1, crx_id)  # SignedData{ crx_id }

    msg = b"CRX3 SignedData\x00" + struct.pack("<I", len(signed_header_data)) \
        + signed_header_data + zip_bytes
    sig = sign(msg, key)

    proof = pb_bytes(1, pub) + pb_bytes(2, sig)
    header = pb_bytes(2, proof) + pb_bytes(10000, signed_header_data)
    crx = b"Cr24" + struct.pack("<I", 3) + struct.pack("<I", len(header)) + header + zip_bytes

    with open(out_crx, "wb") as f:
        f.write(crx)

    eid = ext_id_from_pubkey(pub)
    print("crx      : %s (%d bytes)" % (out_crx, len(crx)))
    print("key      : %s" % key)
    print("ext_id   : %s" % eid)
    print("pubkey   : %s" % hashlib.sha256(pub).hexdigest()[:16])


if __name__ == "__main__":
    main()
