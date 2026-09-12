#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 Gemini Cookie Sync 扩展安装进服务器自带 Chromium（系统级、对所有 profile 生效）。

用法:
  install_ext.py <ext_src_dir> [--root /opt/gw2a-cookie-sync/ext]

做的事:
  0. 清空稳定目录（保留签名私钥，避免旧包被再次打包）
  1. 生成/复用 RSA 密钥（决定扩展 ID，务必保留）
  2. 把公钥以 "key" 字段写入 manifest.json（unpacked 加载也得到同一个 ID）
  3. 打包 CRX3
  4. 生成 update.xml（file:// 更新清单，便于手动安装/更新）
  5. 写入 Chromium 托管策略（只放开 file:// 安装来源）

注意：不要用 ExtensionInstallForcelist。本机 Chromium 没有 Google API key，
无法访问 CRX 更新服务，反而会把扩展判为「blocked by the administrator」，
连 --load-extension 一起堵死。扩展由控制器用 --load-extension 加载。
"""
import argparse
import base64
import json
import os
import shutil
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from pack_crx import ensure_key, pubkey_der, ext_id_from_pubkey, main as pack_main  # noqa: E402

POLICY_DIRS = [
    "/etc/chromium/policies/managed",
    "/etc/opt/chrome/policies/managed",
]


def clean_dir(root):
    """清空目录，但保留签名私钥。"""
    for fn in os.listdir(root):
        if fn == "gw2a-ext.pem":
            continue
        p = os.path.join(root, fn)
        if os.path.isdir(p):
            shutil.rmtree(p, ignore_errors=True)
        else:
            os.remove(p)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("ext_src")
    ap.add_argument("--root", default="/opt/gw2a-cookie-sync/ext")
    ap.add_argument("--name", default="gw2a-cookie-sync")
    args = ap.parse_args()

    root = args.root
    os.makedirs(root, exist_ok=True)
    clean_dir(root)

    # 1) 拷贝扩展源码
    for fn in os.listdir(args.ext_src):
        src = os.path.join(args.ext_src, fn)
        dst = os.path.join(root, fn)
        if os.path.isdir(src):
            shutil.copytree(src, dst)
        else:
            shutil.copy2(src, dst)

    # 2) 密钥 + manifest.key
    key_path = os.path.join(root, "gw2a-ext.pem")
    ensure_key(key_path)
    pub = pubkey_der(key_path)
    eid = ext_id_from_pubkey(pub)
    manifest_path = os.path.join(root, "manifest.json")
    with open(manifest_path, "r", encoding="utf-8") as f:
        manifest = json.load(f)
    manifest["key"] = base64.b64encode(pub).decode("ascii")
    with open(manifest_path, "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
        f.write("\n")

    # 3) 打包 CRX（临时移出私钥，避免把 .pem 打进 zip）
    crx_path = os.path.join(root, args.name + ".crx")
    tmp_key = os.path.join("/tmp", "gw2a-ext-%d.pem" % os.getpid())
    shutil.copy2(key_path, tmp_key)
    os.remove(key_path)
    try:
        sys.argv = ["pack_crx.py", root, crx_path, "--key", tmp_key]
        pack_main()
    finally:
        shutil.copy2(tmp_key, key_path)
        os.remove(tmp_key)

    if not os.path.exists(crx_path):
        raise SystemExit("CRX 打包失败")

    # 4) update.xml
    xml = (
        "<?xml version='1.0' encoding='UTF-8'?>\n"
        "<gupdate xmlns='http://www.google.com/update2/response' protocol='2.0'>\n"
        "  <app appid='%s'>\n"
        "    <updatecheck codebase='file://%s' version='%s' />\n"
        "  </app>\n"
        "</gupdate>\n" % (eid, crx_path, manifest["version"])
    )
    xml_path = os.path.join(root, "update.xml")
    with open(xml_path, "w", encoding="utf-8") as f:
        f.write(xml)

    # 5) 托管策略（只放开安装来源，不用 forcelist）
    policy = {"ExtensionInstallSources": ["file://%s/*" % root]}
    for d in POLICY_DIRS:
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "gw2a-cookie-sync.json"), "w", encoding="utf-8") as f:
            json.dump(policy, f, ensure_ascii=False, indent=2)
            f.write("\n")

    os.chmod(root, 0o755)
    for fn in os.listdir(root):
        try:
            os.chmod(os.path.join(root, fn), 0o644)
        except Exception:
            pass
    try:
        os.chmod(key_path, 0o600)
        os.chmod(crx_path, 0o644)
    except Exception:
        pass

    print("EXT_ID=%s" % eid)
    print("EXT_DIR=%s" % root)
    print("CRX=%s" % crx_path)
    print("UPDATE_XML=%s" % xml_path)
    print("POLICY=%s" % ", ".join(os.path.join(d, "gw2a-cookie-sync.json") for d in POLICY_DIRS))


if __name__ == "__main__":
    main()
