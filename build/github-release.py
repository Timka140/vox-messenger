#!/usr/bin/env python3
"""github-release.py — выкладывает собранный релиз Vox в GitHub Releases.

    python build/github-release.py v0.00.000.0001 ["описание"]

Берёт готовую сборку из .local/release/<version>/ (её делает build/release.sh),
пакует windows-amd64 в .zip и linux-amd64 в .tar.gz, создаёт тег <version> на
текущем коммите (он должен быть уже отправлен в origin) и релиз с архивами.
Повторный запуск дозаливает недостающие архивы в уже созданный релиз.

Токен: переменная GH_TOKEN, иначе сохранённый вход git (git credential fill).
Репозиторий — из адреса origin. Нужен только Python 3 и git.
"""
import json
import os
import re
import subprocess
import sys
import tarfile
import urllib.error
import urllib.parse
import urllib.request
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
API = "https://api.github.com"


def git(*args, stdin=None):
    return subprocess.run(["git", "-C", ROOT, *args], input=stdin, capture_output=True,
                          text=True, check=True).stdout.strip()


def token():
    tok = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if tok:
        return tok
    out = git("credential", "fill", stdin="protocol=https\nhost=github.com\n\n")
    for line in out.splitlines():
        if line.startswith("password="):
            return line[len("password="):]
    sys.exit("нет токена GitHub: задайте GH_TOKEN или войдите в git (git push)")


def repo():
    url = git("remote", "get-url", "origin")
    m = re.search(r"github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?$", url)
    if not m:
        sys.exit("origin не похож на репозиторий GitHub: " + url)
    return m.group(1) + "/" + m.group(2)


def call(method, url, tok, data=None, ctype="application/json"):
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", "Bearer " + tok)
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("User-Agent", "release-script")
    if data is not None:
        req.add_header("Content-Type", ctype)
    try:
        with urllib.request.urlopen(req, timeout=1800) as resp:
            body = resp.read()
            return resp.status, (json.loads(body) if body else None)
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            return e.code, json.loads(body)
        except ValueError:
            return e.code, {"message": body.decode("utf-8", "replace")}


def pack(version, out_dir):
    """Собирает архивы; возвращает список путей."""
    rel = os.path.join(ROOT, ".local", "release", version)
    if not os.path.isdir(rel):
        sys.exit("нет сборки " + rel + " — сначала build/release.sh")
    os.makedirs(out_dir, exist_ok=True)
    archives = []

    win = os.path.join(rel, "windows-amd64")
    if os.path.isdir(win):
        path = os.path.join(out_dir, "Vox-%s-windows-amd64.zip" % version)
        with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as z:
            for base, _, files in os.walk(win):
                for f in sorted(files):
                    full = os.path.join(base, f)
                    z.write(full, os.path.join("Vox", os.path.relpath(full, win)))
        archives.append(path)

    lin = os.path.join(rel, "linux-amd64")
    if os.path.isdir(lin):
        path = os.path.join(out_dir, "Vox-%s-linux-amd64.tar.gz" % version)

        def fix(info):
            # Сборка лежит на NTFS — права выставляем сами.
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            name = os.path.basename(info.name)
            info.mode = 0o755 if info.isdir() or name == "vox" or ".so" in name else 0o644
            return info

        with tarfile.open(path, "w:gz") as t:
            t.add(lin, arcname="vox", filter=fix)
        archives.append(path)

    if not archives:
        sys.exit("в " + rel + " нет ни windows-amd64, ни linux-amd64")
    return archives


def main():
    if len(sys.argv) < 2 or not re.fullmatch(r"v\d+\.\d{2}\.\d{3}\.\d{4}", sys.argv[1]):
        sys.exit("usage: github-release.py v0.00.000.0000 [описание]")
    version = sys.argv[1]
    notes = sys.argv[2] if len(sys.argv) > 2 else ""

    head = git("rev-parse", "HEAD")
    if not git("branch", "-r", "--contains", head):
        sys.exit("коммит %s не отправлен в origin — сначала git push" % head[:7])

    tok, name = token(), repo()
    archives = pack(version, os.path.join(ROOT, ".local", "release", version, "github"))

    code, rel = call("GET", "%s/repos/%s/releases/tags/%s" % (API, name, version), tok)
    if code == 404:
        code, rel = call("POST", "%s/repos/%s/releases" % (API, name), tok, json.dumps({
            "tag_name": version, "target_commitish": head, "name": "Vox " + version, "body": notes,
        }).encode())
        if code != 201:
            sys.exit("не удалось создать релиз: %s %s" % (code, rel.get("message")))
        print("релиз создан:", rel["html_url"])
    elif code == 200:
        print("релиз уже есть:", rel["html_url"])
    else:
        sys.exit("GitHub ответил %s: %s" % (code, rel.get("message")))

    have = {a["name"] for a in rel.get("assets", [])}
    upload = rel["upload_url"].split("{")[0]
    for path in archives:
        fname = os.path.basename(path)
        if fname in have:
            print("уже залит:", fname)
            continue
        size = os.path.getsize(path)
        print("заливаю %s (%.1f МБ)..." % (fname, size / 1e6))
        with open(path, "rb") as f:
            code, res = call("POST", upload + "?name=" + urllib.parse.quote(fname), tok,
                             f.read(), "application/octet-stream")
        if code != 201:
            sys.exit("не удалось залить %s: %s %s" % (fname, code, res.get("message")))
        print("  ", res["browser_download_url"])
    print("готово:", rel["html_url"])


if __name__ == "__main__":
    main()
