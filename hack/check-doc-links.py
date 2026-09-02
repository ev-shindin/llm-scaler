#!/usr/bin/env python3
"""Check every relative markdown link and heading anchor under docs/ and the READMEs,
and every docs/ path named from outside markdown.

Fenced code blocks are skipped: Go generics like `lru.New[podKey, chainNode](size)`
read as a markdown link to any regex that does not know where the fences are, and
a checker that cries wolf gets ignored, which is worse than not having one.
"""
import argparse, re, sys, pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
FENCE = re.compile(r"^\s*(```|~~~)")

def strip_code(text):
    out, inside = [], False
    for line in text.splitlines():
        if FENCE.match(line):
            inside = not inside
            out.append("")
            continue
        out.append("" if inside else line)
    return "\n".join(out)

def slugs(text):
    return {re.sub(r"\s", "-", re.sub(r"[^\w\s-]", "", m.group(2).strip().lower()))
            for m in (re.match(r"^(#{1,6})\s+(.*)$", l) for l in text.splitlines()) if m}

# Hosts that answer 404 to an unauthenticated client on DEEP paths while serving
# the repository root perfectly well. GitHub does this to anything that looks like
# scraping, so a naive external check reports ai-dynamo/dynamo/releases as broken —
# a page that plainly exists. Verified by hand: repo roots return 200 while every
# /issues, /blob and /releases path under them returns 404 from the same client.
#
# Checking these would produce enough false positives to bury the real ones, so the
# external check skips them and says how many it skipped rather than pretending to
# have covered them.
UNVERIFIABLE_HOSTS = ("github.com",)


def check_external(links):
    """Resolve external links over the network. Opt-in: CI has no egress guarantee."""
    import ssl
    import urllib.error
    import urllib.request
    from concurrent.futures import ThreadPoolExecutor

    ctx = ssl.create_default_context()
    checkable = {u: srcs for u, srcs in links.items()
                 if not any(h in u.split("/")[2] for h in UNVERIFIABLE_HOSTS)}
    skipped = len(links) - len(checkable)

    def probe(url):
        req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0 (doc-link-check)"})
        try:
            with urllib.request.urlopen(req, timeout=20, context=ctx) as r:
                return url, r.status
        except urllib.error.HTTPError as e:
            return url, e.code
        except Exception as e:  # DNS, TLS, timeout
            return url, type(e).__name__

    with ThreadPoolExecutor(max_workers=8) as ex:
        results = dict(ex.map(probe, checkable))

    bad = 0
    for url, status in sorted(results.items(), key=lambda kv: str(kv[1])):
        if isinstance(status, int) and 200 <= status < 400:
            continue
        bad += 1
        print(f"BROKEN [{status}] {url}", file=sys.stderr)
        for src in sorted(checkable[url]):
            print(f"         <- {src}", file=sys.stderr)
    print(f"{len(results)} external links checked, {bad} broken "
          f"({skipped} skipped on hosts that 404 unauthenticated deep paths)")
    return bad


def check_doc_mentions():
    """Check docs/ paths named from OUTSIDE markdown: shell, Go, YAML, the Makefile.

    These are the pointers an operator follows out of a log line or an installer
    error, and nothing else checks them. A docs restructure moves the file, every
    markdown link is rewritten because this script insists on it, and the shell
    keeps printing a path that 404s -- which is exactly what happened when
    docs/deployment/ became docs/reference/.

    Only repo-relative mentions count. A docs/ path inside a URL belongs to
    somebody else's repository.
    """
    import subprocess
    listing = subprocess.run(["git", "ls-files"], capture_output=True, text=True, cwd=ROOT)
    files = [f for f in listing.stdout.splitlines() if f and not f.endswith(".md")]
    if not files:
        print("ERROR: no tracked files found, so no doc mentions were checked.",
              file=sys.stderr)
        return 1
    url = re.compile(r"https?://\S+")
    mention = re.compile(r"(?<![\w/.-])docs/[\w./-]+\.(?:md|yaml|yml|py|sh)")
    bad = 0
    for rel in files:
        f = ROOT / rel
        try:
            text = f.read_text(encoding="utf-8", errors="ignore")
        except OSError:
            continue
        for target in sorted(set(mention.findall(url.sub("", text)))):
            if not (ROOT / target).exists():
                print(f"BROKEN MENTION {rel} -> {target}")
                bad += 1
    print(f"{len(files)} non-markdown files scanned for docs/ mentions, {bad} broken")
    return bad


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--external", action="store_true",
                    help="also resolve http(s) links over the network")
    args = ap.parse_args()

    external = {}
    # Every tracked markdown file, not a hand-listed subset: test/e2e/README.md and
    # comparison/ both carried link problems that a docs-only glob never looked at.
    import subprocess
    listing = subprocess.run(
        ["git", "ls-files", "*.md"], capture_output=True, text=True, cwd=ROOT
    )
    files = [ROOT / f for f in listing.stdout.splitlines()]
    # A checker that cannot find the files must not report success. `git ls-files`
    # fails inside WSL for a Windows-created worktree -- .git holds a `C:/...`
    # gitdir that WSL cannot resolve -- and the empty result read as
    # "0 files checked, 0 broken", exit 0. Green, having checked nothing.
    if not files:
        print("ERROR: no tracked markdown files found, so nothing was checked.",
              file=sys.stderr)
        if listing.returncode != 0:
            print("git ls-files failed: " + listing.stderr.strip(), file=sys.stderr)
        return 1
    bad = 0
    for p in files:
        raw = p.read_text(encoding="utf-8")
        prose = strip_code(raw)
        own = slugs(raw)
        for link in re.findall(r"\]\(([^)\s]+)\)", prose):
            if link.startswith(("http", "mailto:")):
                if link.startswith("http"):
                    external.setdefault(link, set()).add(str(p.relative_to(ROOT)).replace("\\", "/"))
                continue
            target, _, anchor = link.partition("#")

            # Link the FOLDER, not its README.md. GitHub picks blob-vs-tree from
            # what the target is: a file gives
            # /blob/main/docs/guides/install-in-namespace/README.md, a directory
            # gives /tree/main/docs/guides/install-in-namespace -- which renders the
            # same README with guide.yaml and the rest of the directory beside it.
            # An anchor is exempt: it addresses a heading inside the file.
            if target.endswith("/README.md") and not anchor:
                print(f"USE FOLDER    {p.relative_to(ROOT)} -> {link} "
                      f"(link {target[:-len('README.md')]} so GitHub opens the directory)")
                bad += 1
                continue

            # A directory without a trailing slash still resolves, but the slash is
            # what makes it obvious to a reader that it is a directory.
            if target and (p.parent / target).is_dir() and not target.endswith("/"):
                print(f"ADD SLASH     {p.relative_to(ROOT)} -> {link} (target is a directory)")
                bad += 1
                continue

            if target:
                f = (p.parent / target).resolve()
                if not f.exists():
                    print(f"BROKEN FILE   {p.relative_to(ROOT)} -> {link}"); bad += 1; continue
                if anchor and f.suffix == ".md" and anchor not in slugs(f.read_text(encoding="utf-8")):
                    print(f"BROKEN ANCHOR {p.relative_to(ROOT)} -> {link}"); bad += 1
            elif anchor and anchor not in own:
                print(f"BROKEN ANCHOR {p.relative_to(ROOT)} -> #{anchor}"); bad += 1
    print(f"{len(files)} files checked, {bad} broken")

    bad += check_doc_mentions()

    if args.external:
        bad += check_external(external)

    return 1 if bad else 0

if __name__ == "__main__":
    sys.exit(main())
