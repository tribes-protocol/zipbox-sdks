"""Create (or reuse) a box, run a command, write and read a file, publish a port, stop it."""

from zipbox import Zipbox

zb = Zipbox()  # ZIPBOX_API_KEY
box = zb.sandboxes.get_or_create("quickstart")
print(box.exec("echo hello from $(hostname)").stdout, end="")
box.files.write("hello.txt", "hi from the SDK\n")
print(box.files.read_text("hello.txt"), end="")
server = box.exec_background("python3 -m http.server 8000", cwd="/root/workspace")
print("preview:", box.preview.create(8000, label="quickstart").url)
server.signal("TERM")
box.stop()
