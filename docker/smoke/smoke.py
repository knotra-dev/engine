"""Exercise package, input and output mounts without a model provider."""

import json
import os
from pathlib import Path

context = json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())
source = Path(context["artifacts"]["source"]["path"]).read_text()
assert source == "container smoke input\n", source

answer = context["values"]["number"] * 2
assert answer == 42, answer

Path("report.txt").write_text(f"{source}answer={answer}\n")
Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text(json.dumps({"answer": answer}))
