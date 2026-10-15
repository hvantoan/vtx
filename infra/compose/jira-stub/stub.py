#!/usr/bin/env python3
"""Deterministic Jira REST stub for webhook E2E tests (VTX-2, JT-01..JT-18).

Issue-ID conventions:
  /rest/api/3/issue/1900NN  -> current status = READY_STATUS (default 10007)
  /rest/api/3/issue/1901NN  -> current status = 10005 (stale: already left)
  /rest/api/3/issue/1907NN  -> READY_STATUS but project key OTHER (identity mismatch)
  /rest/api/3/issue/190429  -> 429 (rate limit)
  /rest/api/3/issue/190500  -> 500
  /rest/api/3/issue/190503  -> first call 503, subsequent calls READY_STATUS
  /rest/api/3/issue/190TO   -> hangs past the client's 3s budget
Anything else -> 404. The response "id" echoes the requested issue ID and
"key" returns the fixture's authoritative key (ISSUE_KEYS) — the backend
re-verifies id/key/project against this view (review P1-2).
"""
import json
import re
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

READY_STATUS = "10007"
STALE_STATUS = "10005"
_seen_503 = set()

ISSUE_RE = re.compile(r"^/rest/api/3/issue/(\d+)")


# Authoritative issue keys per fixture ID, matching the keys the JT harness
# claims in its webhook payloads. `190503*` is the JT-13 retry fixture whose
# ID carries a runtime suffix; its key is always VTX-113.
ISSUE_KEYS = {
    "190001": "VTX-101",
    "190002": "VTX-102",
    "190003": "VTX-103",
    "190004": "VTX-105",
    "190005": "VTX-118",
    "190006": "VTX-106",
    "190007": "VTX-108",
    "190101": "VTX-104",
    "190701": "VTX-107",
}


def issue_key_for(issue_id: str, project_key: str) -> str:
    if issue_id.startswith("190503"):
        return "VTX-113"
    if issue_id in ISSUE_KEYS:
        return ISSUE_KEYS[issue_id]
    # Unknown fixture: deterministic key; a mismatched claim lands as
    # ignored_scope, which is the safe default for unmapped IDs.
    return f"{project_key}-{int(issue_id[-3:])}"


def issue_body(issue_id: str, status_id: str, project_key: str = "VTX") -> bytes:
    return json.dumps({
        "id": issue_id,
        "key": issue_key_for(issue_id, project_key),
        "fields": {
            "status": {"id": status_id, "name": "Ready To Develope" if status_id == READY_STATUS else "In Progress"},
            "project": {"key": project_key},
        },
    }).encode()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _send(self, code: int, body: bytes = b"{}"):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        m = ISSUE_RE.match(self.path.split("?")[0])
        if not m:
            self._send(404)
            return
        issue_id = m.group(1)

        if issue_id.startswith("190429"):
            self._send(429, b'{"errorMessages":["rate limited"]}')
            return
        if issue_id.startswith("190500"):
            self._send(500, b'{"errorMessages":["boom"]}')
            return
        if issue_id.startswith("190503"):
            if issue_id in _seen_503:
                self._send(200, issue_body(issue_id, READY_STATUS))
            else:
                _seen_503.add(issue_id)
                self._send(503)
            return
        if issue_id.startswith("190TO"):
            time.sleep(30)  # far beyond the client's 3s budget
            self._send(200, issue_body(issue_id, READY_STATUS))
            return
        if issue_id.startswith("1907"):
            # Forged-identity fixture: webhook claims project VTX; Jira's
            # authoritative view says the issue belongs to OTHER.
            self._send(200, issue_body(issue_id, READY_STATUS, project_key="OTHER"))
            return
        if issue_id.startswith("1901"):
            self._send(200, issue_body(issue_id, STALE_STATUS))
            return
        if issue_id.startswith("1900"):
            self._send(200, issue_body(issue_id, READY_STATUS))
            return
        self._send(404)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
