#!/usr/bin/env python3
"""Owner-side Telegram bootstrap. Secrets are prompted, never argv or logs."""
import argparse
import getpass
import json
import re
import subprocess
import sys
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("identify", "seal"))
    parser.add_argument("--cert", help="Cluster's public kubeseal certificate")
    parser.add_argument("--namespace", default="teleport-approvals", help="Namespace for the sealed Secret")
    args = parser.parse_args()
    if args.action == "seal" and not args.cert:
        parser.error("seal requires --cert")
    if not sys.stdin.isatty():
        parser.error("run interactively so the token can be entered without echo")
    token = getpass.getpass("Dedicated Telegram bot token: ")
    if not re.fullmatch(r"[0-9]+:[A-Za-z0-9_-]{20,}", token):
        raise ValueError("Invalid token format")
    if args.action == "seal":
        secret = {
            "apiVersion": "v1", "kind": "Secret",
            "metadata": {"name": "telegram-approver", "namespace": args.namespace},
            "stringData": {"token": token},
        }
        result = subprocess.run(
            ["kubeseal", "--cert", args.cert, "--format", "yaml", "--scope", "strict"],
            input=json.dumps(secret), text=True, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, check=False,
        )
        if result.returncode:
            raise ValueError("kubeseal failed; verify the public certificate and executable")
        print(result.stdout, end="")
        return
    request = urllib.request.Request(
        "https://api.telegram.org/bot" + token + "/getUpdates",
        data=json.dumps({"timeout": 0, "allowed_updates": ["message"]}).encode(),
        headers={"Content-Type": "application/json"},
    )
    # Do not follow redirects with credentials embedded in the URL.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None
    with urllib.request.build_opener(NoRedirect).open(request, timeout=15) as response:
        data = json.load(response)
    if not data.get("ok"):
        raise ValueError("Telegram rejected getUpdates")
    ids = set()
    for update in data.get("result", []):
        message = update.get("message", {})
        sender, chat = message.get("from", {}), message.get("chat", {})
        if (message.get("text") == "/start" and chat.get("type") == "private"
                and sender.get("id") == chat.get("id") and not sender.get("is_bot")):
            ids.add(sender["id"])
    if not ids:
        raise ValueError("No private /start found; send /start to the dedicated bot first")
    print("Private /start sender IDs (verify your own conversation):")
    for identity in sorted(ids):
        print(identity)


if __name__ == "__main__":
    try:
        main()
    except (urllib.error.URLError, OSError, ValueError) as error:
        # Network exceptions can include the token-bearing URL. Never print them.
        if isinstance(error, ValueError):
            print(str(error), file=sys.stderr)
        else:
            print("Bootstrap failed; check connectivity, bot token and local tools.", file=sys.stderr)
        sys.exit(1)
