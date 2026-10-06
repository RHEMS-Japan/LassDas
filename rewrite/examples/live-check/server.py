"""The fictional project's service, which the live check example starts.

It stands in for an installation's own service, started from the project's
checkout. It listens on 127.0.0.1 only and answers:

  GET  /health              200 "ok"
  POST /users               201 for {"id": ..., "name": ...}: the user is kept
                            as a file under --data, as a project keeps its
                            data in a database
  GET  /greeting?user=<id>  200 with the first line of src/greeting.txt, ", "
                            and the user's name; 404 for an unknown user; 500
                            when that first line is empty or cannot be read

Once it is ready it writes the port it listens on to --address-file. Its log,
a line per request with the request line and the status, goes to standard
error. --token is not read: it lets whoever started the service recognise the
process again by its arguments.

GREETING_HOLD_FILE, when set, makes /greeting wait for as long as that file
exists, after writing <file>.waiting: a slow page, with which the tests stop a
check while it waits for an answer.
"""
import argparse
import http.server
import json
import os
from pathlib import Path
import re
import time
import urllib.parse

USER_ID = re.compile(r"[A-Za-z0-9-]{1,64}")


def main():
    parser = argparse.ArgumentParser(description="The fictional project's greeting service.")
    parser.add_argument("--root", default=".", help="the project's checkout")
    parser.add_argument("--data", required=True, help="directory of the project's stored data")
    parser.add_argument("--port", type=int, default=0, help="0 takes a free port")
    parser.add_argument("--address-file", required=True, help="where the port is written once ready")
    parser.add_argument("--token", default="", help="not read")
    arguments = parser.parse_args()
    root = Path(arguments.root).resolve()
    users = Path(arguments.data) / "users"
    users.mkdir(parents=True, exist_ok=True)
    hold = os.environ.get("GREETING_HOLD_FILE", "")

    class Handler(http.server.BaseHTTPRequestHandler):
        def reply(self, status, text):
            body = text.encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            url = urllib.parse.urlsplit(self.path)
            if url.path == "/health":
                return self.reply(200, "ok")
            if url.path != "/greeting":
                return self.reply(404, "not found")
            if hold and os.path.exists(hold):
                Path(hold + ".waiting").touch()
                while os.path.exists(hold):
                    time.sleep(0.02)
            user = urllib.parse.parse_qs(url.query).get("user", [""])[0]
            record = users / (user + ".json")
            if not USER_ID.fullmatch(user) or not record.is_file():
                return self.reply(404, "no such user")
            try:
                lines = (root / "src" / "greeting.txt").read_text(encoding="utf-8").splitlines()
            except (OSError, UnicodeDecodeError) as error:
                return self.reply(500, "src/greeting.txt cannot be read: " + type(error).__name__)
            if not lines or not lines[0].strip():
                return self.reply(500, "the first line of src/greeting.txt is empty")
            name = json.loads(record.read_text(encoding="utf-8"))["name"]
            return self.reply(200, lines[0] + ", " + name)

        def do_POST(self):
            if self.path != "/users":
                return self.reply(404, "not found")
            try:
                user = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)))
                identifier, name = user["id"], user["name"]
            except (ValueError, KeyError, TypeError):
                return self.reply(400, 'send {"id": ..., "name": ...}')
            if not isinstance(identifier, str) or not USER_ID.fullmatch(identifier) \
                    or not isinstance(name, str) or not name.strip():
                return self.reply(400, "the id is letters, digits and hyphens; the name is not empty")
            temporary = users / (identifier + ".json.new")
            try:
                temporary.write_text(json.dumps({"id": identifier, "name": name}, ensure_ascii=False),
                                     encoding="utf-8")
                os.replace(temporary, users / (identifier + ".json"))
            except OSError as error:
                return self.reply(500, "the user could not be stored: " + type(error).__name__)
            return self.reply(201, json.dumps({"id": identifier}))

    service = http.server.ThreadingHTTPServer(("127.0.0.1", arguments.port), Handler)
    address = Path(arguments.address_file)
    written = address.with_name(address.name + ".new")
    written.write_text(str(service.server_address[1]), encoding="utf-8")
    os.replace(written, address)
    service.serve_forever()


if __name__ == "__main__":
    main()
