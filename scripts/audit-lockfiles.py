"""Query OSV for both Rust lockfiles. Sends public dependency names/versions only.

Python 3.11+. Findings include every locked target, not just reachable Windows code.
API reference: https://google.github.io/osv.dev/post-v1-querybatch/

A finding stops the audit (exit 1) unless scripts/audit-allowlist.toml accepts it: one entry per
advisory ID, crate and locked version, with the reason and the date somebody last reviewed it.
A different version of the same crate is a new finding. An entry whose advisory no longer shows
up is reported, so that it gets removed; one reviewed more than REVIEW_DAYS ago is reported as
due for review (a warning only: a date must not be what stops a release).

Usage: python scripts/audit-lockfiles.py [--no-allowlist] [--allowlist FILE] [--report FILE]
Exit status: 0 nothing but accepted findings, 1 a finding that is not accepted, 2 a bad allowlist
or OSV could not be reached.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import sys
import time
import tomllib
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_ALLOWLIST = ROOT / "scripts/audit-allowlist.toml"
DEFAULT_REPORT = ROOT / "artifacts/rust-vulnerability-scan.json"
REVIEW_DAYS = 180
ATTEMPTS = 4  # per OSV request; the pause between tries is 5, 10 and 20 s


def warn(message):
    # In GitHub Actions the annotation shows on the run's summary page; elsewhere it is plain text.
    print(f"::warning::{message}" if os.environ.get("GITHUB_ACTIONS") else f"warning: {message}", file=sys.stderr, flush=True)


def load_allowlist(path):
    """The accepted findings as {(advisory ID, crate, version): entry}. Raises ValueError for a bad file."""
    with path.open("rb") as source:
        entries = tomllib.load(source).get("accepted", [])
    accepted = {}
    for number, entry in enumerate(entries, 1):
        where = f"{path.name}, entry {number}"
        for field in ("id", "crate", "version", "reason", "reviewed"):
            if not isinstance(entry.get(field), (str, datetime.date)) or not str(entry[field]).strip():
                raise ValueError(f"{where}: '{field}' is missing")
        try:
            # TOML reads an unquoted date as a date, a quoted one as text.
            entry["reviewed"] = datetime.date.fromisoformat(str(entry["reviewed"]))
        except ValueError:
            raise ValueError(f"{where}: 'reviewed' is not a date (YYYY-MM-DD)") from None
        key = (entry["id"], entry["crate"], entry["version"])
        if key in accepted:
            raise ValueError(f"{where}: {key[0]} for {key[1]} {key[2]} is listed twice")
        accepted[key] = entry
    return accepted


def query_osv(queries):
    """The "results" of one querybatch request, tried again when OSV or the network fails."""
    body = json.dumps({"queries": queries}).encode()
    for attempt in range(1, ATTEMPTS + 1):
        request = urllib.request.Request("https://api.osv.dev/v1/querybatch", body, {"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.load(response)["results"]
        except (urllib.error.URLError, TimeoutError, ConnectionError, json.JSONDecodeError) as error:
            # A 4xx answer is our mistake, not OSV being busy: trying again will not change it.
            if isinstance(error, urllib.error.HTTPError) and error.code < 500 and error.code != 429:
                raise
            if attempt == ATTEMPTS:
                raise
            pause = 5 * 2 ** (attempt - 1)
            warn(f"OSV did not answer ({error}); trying again in {pause} s")
            time.sleep(pause)


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--allowlist", type=Path, default=DEFAULT_ALLOWLIST, help="accepted findings (default: scripts/audit-allowlist.toml)")
    parser.add_argument("--no-allowlist", action="store_true", help="accept nothing, whatever the allowlist says")
    parser.add_argument("--report", type=Path, default=DEFAULT_REPORT, help="where to write the JSON report (default: artifacts/rust-vulnerability-scan.json)")
    arguments = parser.parse_args()
    try:
        accepted = {} if arguments.no_allowlist else load_allowlist(arguments.allowlist)
    except (OSError, ValueError, tomllib.TOMLDecodeError) as error:
        print(f"error: allowlist {arguments.allowlist}: {error}", file=sys.stderr)
        return 2

    packages = set()
    for lock in (ROOT / "Cargo.lock", ROOT / "apps/desktop/src-tauri/Cargo.lock"):
        with lock.open("rb") as source:
            for package in tomllib.load(source)["package"]:
                if package.get("source", "").startswith("registry+"):
                    packages.add((package["name"], package["version"]))
    findings = []
    ordered = sorted(packages)
    try:
        for start in range(0, len(ordered), 100):
            batch = ordered[start:start + 100]
            queries = [{"package": {"ecosystem": "crates.io", "name": name}, "version": version} for name, version in batch]
            pending = list(enumerate(queries))
            while pending:
                results = query_osv([query for _, query in pending])
                if len(results) != len(pending):
                    raise RuntimeError("Incomplete OSV response")
                next_page = []
                for (index, query), result in zip(pending, results):
                    name, version = batch[index]
                    for advisory in result.get("vulns", []):
                        entry = accepted.get((advisory["id"], name, version))
                        finding = {"name": name, "version": version, "id": advisory["id"], "url": "https://osv.dev/vulnerability/" + advisory["id"], "accepted": entry is not None}
                        if entry:
                            finding["reason"] = entry["reason"]
                        findings.append(finding)
                        print(json.dumps(finding), flush=True)
                    if result.get("next_page_token"):
                        next_page.append((index, {**query, "page_token": result["next_page_token"]}))
                pending = next_page
    except (OSError, ValueError, RuntimeError, KeyError) as error:
        # urllib's errors are OSErrors. A scan that could not finish proves nothing: it does not pass.
        print(f"error: the OSV scan did not finish: {error}", file=sys.stderr)
        return 2

    # Entries that match nothing are never a failure, but they must not pile up unnoticed.
    seen = {(finding["id"], finding["name"], finding["version"]) for finding in findings}
    today = datetime.date.today()
    for key, entry in sorted(accepted.items()):
        if key not in seen:
            warn(f"allowlist entry {key[0]} for {key[1]} {key[2]} matches no finding: remove it from {arguments.allowlist.name}")
        elif (today - entry["reviewed"]).days > REVIEW_DAYS:
            warn(f"allowlist entry {key[0]} for {key[1]} {key[2]} was last reviewed on {entry['reviewed']}: review it again")
    rejected = [finding for finding in findings if not finding["accepted"]]
    report = {"checked_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "source": "https://api.osv.dev/v1/querybatch", "package_versions": len(ordered), "findings": findings}
    arguments.report.parent.mkdir(parents=True, exist_ok=True)
    arguments.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"Checked {len(ordered)} locked Rust package versions; {len(findings)} advisory matches, {len(findings) - len(rejected)} accepted by the allowlist, {len(rejected)} not. Report: {arguments.report}")
    for finding in rejected:
        print(f"not accepted: {finding['id']} in {finding['name']} {finding['version']}: {finding['url']}", file=sys.stderr)
    return 1 if rejected else 0


if __name__ == "__main__":
    sys.exit(main())
