"""Start a dev server that outlives this script, publish it, and re-attach to its logs later.

python long_running_dev_server.py start   # prints the command id and URL
python long_running_dev_server.py logs <command-id> [cursor]
"""

import sys

from zipbox import Zipbox

BOX = "dev-server"


def start(zb: Zipbox) -> None:
    box = zb.sandboxes.get_or_create(BOX)
    box.files.write("site/index.html", "<h1>served from a Zipbox</h1>\n")
    command = box.exec_background("python3 -m http.server 8080", cwd="site")
    preview = box.preview.create(8080, label="site")
    print(f"command {command.id} serving {preview.url}")


def logs(zb: Zipbox, command_id: str, since: str | None) -> None:
    box = zb.sandboxes.get_or_create(BOX)
    for event in box.commands.events(command_id, since=since):
        if event.data is not None:
            print(event.data, end="")
        if event.type == "exit":
            print(f"[exited {event.exit_code}]")
        # event.cursor is where to resume if this script is interrupted


if __name__ == "__main__":
    client = Zipbox()
    if sys.argv[1:2] == ["logs"]:
        logs(client, sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else None)
    else:
        start(client)
