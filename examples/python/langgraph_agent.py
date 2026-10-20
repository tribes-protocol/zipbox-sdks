"""A LangGraph ReAct agent with a Zipbox box as its computer.

pip install zipbox langgraph langchain-openai
"""

from langchain_core.tools import tool
from langgraph.prebuilt import create_react_agent

from zipbox import Zipbox

box = Zipbox().sandboxes.get_or_create("langgraph-agent")


@tool
def shell(command: str) -> str:
    """Run a bash command on the box (cwd /root/workspace)."""
    result = box.exec(command)
    return f"exit {result.exit_code}\n{(result.stdout + result.stderr)[-8000:]}"


@tool
def write_file(path: str, content: str) -> str:
    """Write a file on the box."""
    box.files.write(path, content)
    return f"wrote {path}"


@tool
def publish(port: int) -> str:
    """Publish a port on the box at a public HTTPS URL."""
    return box.preview.create(port).url


if __name__ == "__main__":
    agent = create_react_agent("openai:gpt-4o-mini", tools=[shell, write_file, publish])
    task = "Write a small Flask hello-world app, run it on port 5000 in the background, publish it."
    for step in agent.stream({"messages": [("user", task)]}, stream_mode="values"):
        step["messages"][-1].pretty_print()
